# Copyright Daytona Platforms Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from daytona.common.errors import DaytonaError


def _build_multipart_body(
    boundary: bytes,
    *,
    name: str,
    filename: str,
    payload: bytes,
    content_type: str = "application/octet-stream",
) -> bytes:
    return b"".join(
        [
            b"--" + boundary + b"\r\n",
            f'Content-Disposition: form-data; name="{name}"; filename="{filename}"\r\n'.encode("utf-8"),
            f"Content-Type: {content_type}\r\n\r\n".encode("utf-8"),
            payload,
            b"\r\n--" + boundary + b"--\r\n",
        ]
    )


class _SyncStreamResponse:
    def __init__(self, chunks: list[bytes], boundary: bytes):
        self._chunks = chunks
        self.headers = {"Content-Type": f'multipart/form-data; boundary={boundary.decode("utf-8")}'}

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        return False

    def raise_for_status(self):
        return None

    def iter_bytes(self, _chunk_size):
        return iter(self._chunks)


class _SyncStreamClient:
    def __init__(self, response: _SyncStreamResponse):
        self._response = response
        self.stream_args: tuple[object, ...] | None = None
        self.stream_kwargs: dict[str, object] | None = None
        self.closed = False

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        self.close()
        return False

    def stream(self, *args, **kwargs):
        self.stream_args = args
        self.stream_kwargs = kwargs
        return self._response

    def close(self):
        self.closed = True


class _AsyncStreamContent:
    def __init__(self, chunks: list[bytes]):
        self._chunks = chunks

    async def iter_chunked(self, _chunk_size):
        for chunk in self._chunks:
            yield chunk


class _AsyncStreamResponse:
    def __init__(self, chunks: list[bytes], boundary: bytes):
        self._chunks = chunks
        self.headers = {"Content-Type": f'multipart/form-data; boundary={boundary.decode("utf-8")}'}
        self.content = _AsyncStreamContent(chunks)

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc, tb):
        return False

    def raise_for_status(self):
        return None


class _AsyncStreamClient:
    def __init__(self, response: _AsyncStreamResponse):
        self._response = response
        self.stream_args: tuple[object, ...] | None = None
        self.stream_kwargs: dict[str, object] | None = None
        self.closed = False

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc, tb):
        await self.close()
        return False

    def request(self, *args, **kwargs):
        self.stream_args = args
        self.stream_kwargs = kwargs
        return self._response

    async def close(self):
        self.closed = True


class TestSyncFileSystem:
    def _make_fs(self):
        from daytona._sync.filesystem import FileSystem

        mock_api = MagicMock()
        return FileSystem(mock_api, http_client=MagicMock()), mock_api

    def test_create_folder(self):
        fs, api = self._make_fs()
        api.create_folder.return_value = None
        fs.create_folder("workspace/data", "755")
        api.create_folder.assert_called_once_with(path="workspace/data", mode="755")

    def test_delete_file(self):
        fs, api = self._make_fs()
        api.delete_file.return_value = None
        fs.delete_file("workspace/file.txt")
        api.delete_file.assert_called_once_with(path="workspace/file.txt", recursive=False)

    def test_delete_file_recursive(self):
        fs, api = self._make_fs()
        api.delete_file.return_value = None
        fs.delete_file("workspace/dir", recursive=True)
        api.delete_file.assert_called_once_with(path="workspace/dir", recursive=True)

    def test_find_files(self):
        fs, api = self._make_fs()
        mock_match = MagicMock()
        mock_match.file = "src/main.py"
        mock_match.line = 10
        mock_match.content = "TODO: fix this"
        api.find_in_files.return_value = [mock_match]
        result = fs.find_files("workspace/src", "TODO:")
        assert len(result) == 1
        api.find_in_files.assert_called_once_with(path="workspace/src", pattern="TODO:")

    def test_get_file_info(self):
        fs, api = self._make_fs()
        mock_info = MagicMock()
        mock_info.name = "file.txt"
        mock_info.is_dir = False
        mock_info.size = 1024
        api.get_file_info.return_value = mock_info
        result = fs.get_file_info("workspace/file.txt")
        assert result.name == "file.txt"
        assert result.size == 1024

    def test_list_files(self):
        fs, api = self._make_fs()
        mock_file = MagicMock()
        mock_file.name = "test.py"
        api.list_files.return_value = [mock_file]
        result = fs.list_files("workspace")
        assert len(result) == 1
        assert result[0].name == "test.py"

    def test_move_files(self):
        fs, api = self._make_fs()
        api.move_file.return_value = None
        fs.move_files("old/path.txt", "new/path.txt")
        api.move_file.assert_called_once_with(source="old/path.txt", destination="new/path.txt")

    def test_replace_in_files(self):
        fs, api = self._make_fs()
        mock_result = MagicMock()
        mock_result.file = "src/main.py"
        mock_result.success = True
        api.replace_in_files.return_value = [mock_result]
        results = fs.replace_in_files(["src/main.py"], "old_func", "new_func")
        assert len(results) == 1

    def test_search_files(self):
        fs, api = self._make_fs()
        mock_response = MagicMock()
        mock_response.files = ["a.py", "b.py"]
        api.search_files.return_value = mock_response
        result = fs.search_files("workspace", "*.py")
        assert len(result.files) == 2

    def test_set_file_permissions(self):
        fs, api = self._make_fs()
        api.set_file_permissions.return_value = None
        fs.set_file_permissions("workspace/script.sh", mode="755", owner="daytona")
        api.set_file_permissions.assert_called_once_with(
            path="workspace/script.sh", mode="755", owner="daytona", group=None
        )

    def test_download_file_returns_bytes(self):
        fs, _api = self._make_fs()
        fs.download_files = MagicMock(return_value=[MagicMock(error=None, result="hello")])

        assert fs.download_file("workspace/file.txt") == b"hello"

    def test_download_file_raises_when_response_has_error(self):
        fs, _api = self._make_fs()
        fs.download_files = MagicMock(return_value=[MagicMock(error="missing", error_details=None)])

        with pytest.raises(DaytonaError, match="missing"):
            fs.download_file("workspace/file.txt")

    def test_download_file_stream_yields_chunks(self):
        from daytona._sync.filesystem import FileSystem

        mock_api = MagicMock()
        remote_path = "workspace/file.txt"
        boundary = b"sync-boundary"
        payload = b"hello world"
        multipart_body = _build_multipart_body(boundary, name="file", filename=remote_path, payload=payload)
        payload_start = multipart_body.index(payload)
        chunks = [
            multipart_body[: payload_start + 5],
            multipart_body[payload_start + 5 : payload_start + 9],
            multipart_body[payload_start + 9 :],
        ]
        client = _SyncStreamClient(_SyncStreamResponse(chunks, boundary))
        mock_api._download_files_serialize = MagicMock(
            return_value=(
                "POST",
                "https://download",
                {"Authorization": "Bearer token"},
                {"paths": [remote_path]},
            )
        )
        fs = FileSystem(mock_api, http_client=client)

        streamed_chunks = list(fs.download_file_stream(remote_path))

        assert streamed_chunks == [b"hello", b" wor", b"ld"]
        assert client.stream_args == ("POST", "https://download")
        from daytona.internal.http_client import request_timeout

        assert client.stream_kwargs == {
            "json": {"paths": [remote_path]},
            "headers": {"Authorization": "Bearer token"},
            "timeout": request_timeout(30 * 60),
        }

    def test_download_file_stream_raises_on_error_part(self):
        from daytona._sync.filesystem import FileSystem

        mock_api = MagicMock()
        remote_path = "workspace/missing.txt"
        boundary = b"sync-boundary"
        error_payload = b'{"message":"missing","statusCode":404,"code":"not_found"}'
        multipart_body = _build_multipart_body(
            boundary,
            name="error",
            filename=remote_path,
            payload=error_payload,
            content_type="application/json",
        )
        client = _SyncStreamClient(_SyncStreamResponse([multipart_body], boundary))
        mock_api._download_files_serialize = MagicMock(
            return_value=(
                "POST",
                "https://download",
                {"Authorization": "Bearer token"},
                {"paths": [remote_path]},
            )
        )
        fs = FileSystem(mock_api, http_client=client)

        with pytest.raises(DaytonaError, match="missing") as exc_info:
            list(fs.download_file_stream(remote_path))

        assert exc_info.value.status_code == 404
        assert exc_info.value.error_code == "not_found"


class TestAsyncFileSystem:
    def _make_fs(self):
        from daytona._async.filesystem import AsyncFileSystem

        mock_api = AsyncMock()
        return AsyncFileSystem(mock_api), mock_api

    @pytest.mark.asyncio
    async def test_create_folder(self):
        fs, api = self._make_fs()
        await fs.create_folder("workspace/data", "755")
        api.create_folder.assert_called_once_with(path="workspace/data", mode="755")

    @pytest.mark.asyncio
    async def test_delete_file(self):
        fs, api = self._make_fs()
        await fs.delete_file("workspace/file.txt")
        api.delete_file.assert_called_once_with(path="workspace/file.txt", recursive=False)

    @pytest.mark.asyncio
    async def test_find_files(self):
        fs, api = self._make_fs()
        mock_match = MagicMock()
        api.find_in_files.return_value = [mock_match]
        result = await fs.find_files("workspace", "TODO")
        assert len(result) == 1

    @pytest.mark.asyncio
    async def test_get_file_info(self):
        fs, api = self._make_fs()
        mock_info = MagicMock(name="file.txt", is_dir=False, size=512)
        api.get_file_info.return_value = mock_info
        result = await fs.get_file_info("workspace/file.txt")
        assert result is not None

    @pytest.mark.asyncio
    async def test_list_files(self):
        fs, api = self._make_fs()
        api.list_files.return_value = [MagicMock()]
        result = await fs.list_files("workspace")
        assert len(result) == 1

    @pytest.mark.asyncio
    async def test_move_files(self):
        fs, api = self._make_fs()
        await fs.move_files("src.txt", "dst.txt")
        api.move_file.assert_called_once_with(source="src.txt", destination="dst.txt")

    @pytest.mark.asyncio
    async def test_search_files(self):
        fs, api = self._make_fs()
        mock_resp = MagicMock()
        mock_resp.files = ["a.py"]
        api.search_files.return_value = mock_resp
        result = await fs.search_files("workspace", "*.py")
        assert len(result.files) == 1

    @pytest.mark.asyncio
    async def test_set_file_permissions(self):
        fs, api = self._make_fs()
        await fs.set_file_permissions("script.sh", mode="755")
        api.set_file_permissions.assert_called_once_with(path="script.sh", mode="755", owner=None, group=None)

    @pytest.mark.asyncio
    async def test_download_file_returns_bytes(self):
        fs, _api = self._make_fs()
        fs.download_files = AsyncMock(return_value=[MagicMock(error=None, result="hello")])

        assert await fs.download_file("workspace/file.txt") == b"hello"

    @pytest.mark.asyncio
    async def test_download_file_raises_when_response_has_error(self):
        fs, _api = self._make_fs()
        fs.download_files = AsyncMock(return_value=[MagicMock(error="missing", error_details=None)])

        with pytest.raises(DaytonaError, match="missing"):
            await fs.download_file("workspace/file.txt")

    @pytest.mark.asyncio
    async def test_download_file_stream_yields_chunks(self):
        from daytona._async.filesystem import AsyncFileSystem

        mock_api = AsyncMock()
        remote_path = "workspace/file.txt"
        boundary = b"async-boundary"
        payload = b"hello world"
        multipart_body = _build_multipart_body(boundary, name="file", filename=remote_path, payload=payload)
        payload_start = multipart_body.index(payload)
        chunks = [
            multipart_body[: payload_start + 5],
            multipart_body[payload_start + 5 : payload_start + 9],
            multipart_body[payload_start + 9 :],
        ]
        client = _AsyncStreamClient(_AsyncStreamResponse(chunks, boundary))
        mock_api._download_files_serialize = MagicMock(
            return_value=(
                "POST",
                "https://download",
                {"Authorization": "Bearer token"},
                {"paths": [remote_path]},
            )
        )
        mock_api.api_client.http_session = client
        fs = AsyncFileSystem(mock_api)

        streamed_chunks = [chunk async for chunk in await fs.download_file_stream(remote_path)]

        assert streamed_chunks == [b"hello", b" wor", b"ld"]
        assert client.stream_args == ("POST", "https://download")
        from daytona.internal.http_client import aiohttp_request_timeout

        assert client.stream_kwargs == {
            "json": {"paths": [remote_path]},
            "headers": {"Authorization": "Bearer token"},
            "timeout": aiohttp_request_timeout(30 * 60),
        }

    @pytest.mark.asyncio
    async def test_download_file_stream_raises_on_error_part(self):
        from daytona._async.filesystem import AsyncFileSystem

        mock_api = AsyncMock()
        remote_path = "workspace/missing.txt"
        boundary = b"async-boundary"
        error_payload = b'{"message":"missing","statusCode":404,"code":"not_found"}'
        multipart_body = _build_multipart_body(
            boundary,
            name="error",
            filename=remote_path,
            payload=error_payload,
            content_type="application/json",
        )
        client = _AsyncStreamClient(_AsyncStreamResponse([multipart_body], boundary))
        mock_api._download_files_serialize = MagicMock(
            return_value=(
                "POST",
                "https://download",
                {"Authorization": "Bearer token"},
                {"paths": [remote_path]},
            )
        )
        mock_api.api_client.http_session = client
        fs = AsyncFileSystem(mock_api)

        with pytest.raises(DaytonaError, match="missing") as exc_info:
            [chunk async for chunk in await fs.download_file_stream(remote_path)]

        assert exc_info.value.status_code == 404
        assert exc_info.value.error_code == "not_found"
