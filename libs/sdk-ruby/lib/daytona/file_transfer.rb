# Copyright Daytona Platforms Inc.
# SPDX-License-Identifier: Apache-2.0

# frozen_string_literal: true

require 'json'
require 'typhoeus'

module Daytona
  class MultipartDownloadStreamParser
    attr_reader :error_message
    attr_writer :boundary_token

    def initialize(&on_file_chunk)
      @on_file_chunk = on_file_chunk
      @boundary_token = nil
      @buffer = String.new.b
      @state = :preamble
      @part_name = nil
      @error_buffer = String.new.b
    end

    def <<(chunk)
      @buffer << chunk.b
      process!
    end

    def finish!
      process!

      return if @state == :done || @buffer.empty?

      emit(@buffer)
      finalize_part!
      @buffer = String.new.b
      @state = :done
    end

    private

    def process!
      loop do
        advanced = case @state
                   when :preamble then consume_preamble?
                   when :headers then consume_headers?
                   when :body then consume_body?
                   else false
                   end

        break unless advanced
      end
    end

    def consume_preamble?
      start_marker = "#{boundary}\r\n".b
      index = @buffer.index(start_marker)
      return retain_tail?(start_marker.bytesize - 1) unless index

      @buffer = remaining_bytes(index + start_marker.bytesize)
      @state = :headers
      true
    end

    def consume_headers?
      separator = "\r\n\r\n".b
      index = @buffer.index(separator)
      return false unless index

      headers = @buffer.byteslice(0, index)
      @buffer = remaining_bytes(index + separator.bytesize)
      @part_name = headers[/Content-Disposition:\s*[^\r\n]*\bname="([^"]+)"/i, 1]
      raise Sdk::Error, 'Invalid multipart response' if @part_name.nil?

      @state = :body
      true
    end

    def consume_body? # rubocop:disable Metrics/AbcSize, Metrics/MethodLength
      marker = "\r\n#{boundary}".b
      index = @buffer.index(marker)

      if index
        emit(@buffer.byteslice(0, index))
        @buffer = remaining_bytes(index + marker.bytesize)
        finalize_part!
        @state = :done
        return true
      end

      flushable = @buffer.bytesize - marker.bytesize + 1
      return false if flushable <= 0

      emit(@buffer.byteslice(0, flushable))
      @buffer = remaining_bytes(flushable)
      false
    end

    def emit(data)
      return if data.nil? || data.empty?

      case @part_name
      when 'file'
        @on_file_chunk.call(data)
      when 'error'
        @error_buffer << data
      end
    end

    def finalize_part!
      return unless @part_name == 'error'

      @error_message = extract_error_message(@error_buffer)
    end

    def extract_error_message(payload)
      parsed = JSON.parse(payload)
      parsed['message'] || parsed['error'] || payload
    rescue JSON::ParserError
      payload
    end

    def retain_tail?(size)
      @buffer = @buffer.byteslice(-size, size) || String.new.b if size.positive? && @buffer.bytesize > size
      false
    end

    def remaining_bytes(offset)
      @buffer.byteslice(offset, @buffer.bytesize - offset) || String.new.b
    end

    def boundary
      "--#{@boundary_token}".b
    end
  end

  module FileTransfer
    def self.extract_multipart_boundary(content_type)
      match = content_type&.match(/boundary=(?:"([^"]+)"|([^;]+))/i)
      return unless match

      match.captures.compact.first
    end

    # rubocop:disable Metrics/AbcSize, Metrics/MethodLength
    def self.stream_download(api_client:, remote_path:, timeout:, on_progress: nil, &block)
      config = api_client.config
      bytes_received = 0
      wrapped_block = if on_progress
                        proc do |chunk|
                          bytes_received += chunk.bytesize
                          on_progress.call(bytes_received)
                          block.call(chunk)
                        end
                      else
                        block
                      end
      parser = MultipartDownloadStreamParser.new(&wrapped_block)
      response = nil

      request = Typhoeus::Request.new(
        "#{config.base_url}/files/bulk-download",
        method: :post,
        headers: api_client.default_headers.dup.merge(
          'Accept' => 'multipart/form-data',
          'Content-Type' => 'application/json'
        ),
        body: JSON.generate(paths: [remote_path]),
        timeout: timeout,
        ssl_verifypeer: config.verify_ssl,
        ssl_verifyhost: config.verify_ssl_host ? 2 : 0
      )

      request.on_headers do |stream_response|
        boundary = extract_multipart_boundary(stream_response.headers['Content-Type'])
        raise Sdk::Error, 'Missing multipart boundary in download response' unless boundary

        parser.boundary_token = boundary
      end

      request.on_body do |chunk|
        parser << chunk
      end

      request.on_complete do |completed_response|
        response = completed_response
        parser.finish!
      end

      request.run

      raise Sdk::Error, parser.error_message if parser.error_message
      raise Sdk::Error, "HTTP #{response.code}" if response && !response.success?
    end
    # rubocop:enable Metrics/AbcSize, Metrics/MethodLength
  end
end
