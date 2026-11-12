from __future__ import annotations

import asyncio
import concurrent.futures
import threading
from contextlib import AsyncExitStack, suppress

from desktop.bridge.device import DesktopDeviceError


class IOSDeviceError(DesktopDeviceError):
    def __init__(self, code, message):
        self.code = code
        super().__init__(message)


class USBTransport:
    """Keep USB, optional XCTest, and all async clients on one owned event loop."""

    def __init__(self, udid=None, xctrunner=None, timeout=25):
        self.timeout = timeout
        self.loop = asyncio.new_event_loop()
        self.thread = threading.Thread(target=self._serve_loop, daemon=True, name="ios-usb")
        self.thread.start()
        self.stack = AsyncExitStack()
        self.runner = None
        self.client = None
        self.close_future = None
        try:
            self._run(self._connect(udid, xctrunner), 65)
        except Exception:
            self.close()
            raise

    def _serve_loop(self):
        asyncio.set_event_loop(self.loop)
        try:
            self.loop.run_forever()
        finally:
            self.loop.close()

    def _run(self, coroutine, timeout=None):
        future = asyncio.run_coroutine_threadsafe(coroutine, self.loop)
        try:
            return future.result(timeout=timeout or self.timeout)
        except concurrent.futures.TimeoutError as exc:
            future.cancel()
            raise IOSDeviceError("device_timeout", "iOS request timed out; an input request may have been delivered. Inspect the device before retrying.") from exc

    async def _connect(self, udid, xctrunner):
        from pymobiledevice3.lockdown import create_using_usbmux
        from pymobiledevice3.services.wda import WdaServiceClient
        from pymobiledevice3.usbmux import list_devices

        try:
            if not udid:
                devices = [device for device in await list_devices() if device.is_usb]
                if len(devices) != 1:
                    raise IOSDeviceError("device_selection_required", "Connect one USB iPhone or select it explicitly with --udid.")
                udid = devices[0].serial
            lockdown = await self.stack.enter_async_context(await create_using_usbmux(
                serial=udid, autopair=False, connection_type="USB"))
            if not await lockdown.get_developer_mode_status():
                raise IOSDeviceError("developer_mode_required", "Enable Developer Mode on the iPhone before starting the bridge.")
            self.version = lockdown.product_version
            self.client = WdaServiceClient(lockdown, timeout=self.timeout)
            if xctrunner:
                from pymobiledevice3.remote.rsd_tunnel import PreferredRsdTunnel
                from pymobiledevice3.services.dvt.testmanaged.xcuitest import TestConfig, XCUITestService

                # macOS's native developer tunnel completes XCTest handshakes
                # that may stall on the userspace transport after reconnecting.
                rsd = await self.stack.enter_async_context(PreferredRsdTunnel(
                    serial=lockdown.udid, autopair=False, prefer_native=True))
                config = await TestConfig.create_for(rsd, runner_bundle_id=xctrunner)
                self.runner = asyncio.create_task(XCUITestService(rsd).run(config))
                deadline = self.loop.time() + 30
                while True:
                    if self.runner.done():
                        self.runner.result()
                        raise IOSDeviceError("wda_unavailable", "The WDA test runner exited before startup completed.")
                    try:
                        await asyncio.wait_for(self.client.get_status(), 2)
                        break
                    except Exception:
                        if self.loop.time() >= deadline:
                            raise IOSDeviceError("wda_unavailable", "WDA did not start. Unlock the iPhone and verify the runner's signing and developer authorization.")
                        await asyncio.sleep(0.25)
            else:
                await asyncio.wait_for(self.client.get_status(), 5)
        except IOSDeviceError:
            raise
        except Exception as exc:
            raise IOSDeviceError("device_unavailable", "Cannot connect to iPhone/WDA. Check USB, Trust This Computer, Developer Mode, and the installed/running WDA runner.") from exc

    def request(self, method, path, payload=None):
        return self._run(self._request(method, path, payload), self.timeout + 1)

    async def _request(self, method, path, payload):
        if self.runner is not None and self.runner.done():
            raise IOSDeviceError("wda_unavailable", "WDA stopped; restart the bridge after checking the device.")
        try:
            # pymobiledevice3 11.52.1 exposes the complete WDA HTTP transport here;
            # its public convenience API does not include alert commands.
            data = await asyncio.wait_for(self.client._request_json(method, path, payload), self.timeout)
        except TimeoutError as exc:
            raise IOSDeviceError("device_timeout", "WDA timed out; input may have been delivered. Inspect the device before retrying.") from exc
        except Exception as exc:
            message = str(exc).lower()
            if ("/alert/" in path and any(text in message for text in (
                    "no alert is open", "no such alert", "modal dialog when one was not open"))):
                raise IOSDeviceError("no_alert", "No alert is open.") from exc
            detail = " Input may have been delivered; inspect the device before retrying." if method != "GET" else " Check the USB connection and WDA session."
            raise IOSDeviceError("wda_request_failed", "WDA request failed." + detail) from exc
        value = data.get("value")
        if isinstance(value, dict) and value.get("error"):
            raise IOSDeviceError("no_alert" if value["error"] == "no such alert" else "wda_request_failed", str(value.get("message") or value["error"]))
        return data

    async def _close(self):
        try:
            if self.runner is not None:
                self.runner.cancel()
                with suppress(asyncio.CancelledError, Exception):
                    await asyncio.wait_for(asyncio.shield(self.runner), 2)
        finally:
            try:
                await asyncio.wait_for(self.stack.aclose(), 5)
            finally:
                current = asyncio.current_task()
                pending = [task for task in asyncio.all_tasks() if task is not current and not task.done()]
                for task in pending:
                    if not task.cancelling():
                        task.cancel()
                await asyncio.gather(*pending, return_exceptions=True)

    def close(self):
        if not self.loop.is_running():
            return
        if self.close_future is None:
            self.close_future = asyncio.run_coroutine_threadsafe(self._close(), self.loop)
            # Keep the loop alive even if the caller's deadline expires: the
            # coroutine must finish cancelling tasks and releasing the tunnel.
            self.close_future.add_done_callback(lambda _: self.loop.call_soon_threadsafe(self.loop.stop))
        try:
            self.close_future.result(timeout=12)
        except concurrent.futures.TimeoutError as exc:
            raise IOSDeviceError("shutdown_timeout", "iOS shutdown is still releasing connections in the background.") from exc
        finally:
            self.thread.join(timeout=1)
