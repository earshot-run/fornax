"""Optional Chromium checks for chat persistence, streams, search and export.

Run a freshly built studio with an isolated FORNAX_HOME; pass --url and --home.
The real embedded frontend uses deterministic API and streaming fixtures.
"""
import argparse
import asyncio
import json
from pathlib import Path
from urllib.parse import parse_qs, urlparse

from playwright.async_api import async_playwright, expect


async def check(url, home, chromium):
    async with async_playwright() as p:
        browser = await p.chromium.launch(executable_path=chromium)
        context = await browser.new_context(viewport={"width": 1440, "height": 1000})
        await context.add_cookies([{
            "name": "fornax_studio", "value": (home / "server.key").read_text().strip(),
            "url": url, "httpOnly": True,
        }])
        page = await context.new_page()
        errors, saves, saved, deleted = [], [], {}, []
        page.on("pageerror", lambda e: errors.append(str(e)))
        save_started, release_save = asyncio.Event(), asyncio.Event()
        block_save, fail_save, fail_delete, fail_history = True, False, False, False
        old_search = asyncio.Event()
        await page.add_init_script("""
            window.EventSource = class { close() {} };
            const realFetch = window.fetch.bind(window);
            window.fetch = (input, init) => {
              if (input !== '/api/chat') return realFetch(input, init);
              return Promise.resolve(new Response(new ReadableStream({
                start(controller) {
                  window.chatStream = controller;
                  init.signal.addEventListener('abort', () => controller.error(new DOMException('Stopped', 'AbortError')), {once:true});
                }
              }), {headers:{'Content-Type':'text/event-stream'}}));
            };
            window.pushChat = text => chatStream.enqueue(new TextEncoder().encode(text));
        """)

        def summaries(query=""):
            return [
                {"id": cid, "title": conv["title"], "model": conv["model"],
                 "updated": conv["updated"], "count": len(conv["messages"])}
                for cid, conv in reversed(list(saved.items()))
                if not query or query.lower() in json.dumps(conv, ensure_ascii=False).lower()
            ]

        async def api(route):
            nonlocal block_save
            request = route.request
            path = urlparse(request.url).path
            query = parse_qs(urlparse(request.url).query).get("q", [""])[0]
            body = request.post_data_json if request.post_data else None
            status, response = 200, {}
            if path == "/api/models":
                response = [{"id": "chat-1", "name": "Fixture model", "kind": "text", "chat": True, "bytes": 1, "fit": "fits"}]
            elif path == "/api/chat/status" or path == "/api/chat/load":
                response = {"state": "ready", "model": "chat-1"}
            elif path == "/api/chats":
                if query == "old":
                    old_search.set()
                    await asyncio.sleep(.4)
                if fail_history:
                    status, response = 503, "History is offline"
                else:
                    response = summaries(query)
            elif path.startswith("/api/chats/"):
                cid = path.rsplit("/", 1)[1]
                if request.method == "PUT":
                    saves.append((cid, body))
                    if block_save:
                        block_save = False
                        save_started.set()
                        await release_save.wait()
                    if fail_save:
                        status, response = 503, "Disk is temporarily unavailable"
                    else:
                        response = {**body, "id": cid, "created": "2026-10-01T00:00:00Z", "updated": "2026-10-02T00:00:00Z"}
                        saved[cid] = response
                elif request.method == "DELETE":
                    if fail_delete:
                        status, response = 503, "Cannot delete right now"
                    else:
                        deleted.append(cid)
                        saved.pop(cid, None)
                        status, response = 204, ""
                elif request.method == "PATCH":
                    saved[cid]["title"] = body["title"]
                    response = saved[cid]
                else:
                    response = saved[cid]
            else:
                await route.continue_()
                return
            try:
                await route.fulfill(status=status, content_type="application/json" if not isinstance(response, str) else "text/plain", body=json.dumps(response) if not isinstance(response, str) else response)
            except Exception:
                if query != "old":
                    raise

        await page.route("**/api/**", api)
        await page.goto(url + "/#chat")
        await expect(page.locator(".chat-title")).to_have_text("New chat")
        message = page.locator(".chat-input")

        async def send(text):
            await message.fill(text)
            await page.get_by_title("Send (Enter)", exact=True).click()
            await expect(page.get_by_title("Stop (Esc)", exact=True)).to_be_visible()

        async def chunk(text):
            await page.evaluate("text => pushChat(text)", "data: " + json.dumps({"choices": [{"delta": {"content": text}}]}) + "\n\n")

        async def done():
            await page.evaluate("pushChat('data: [DONE]\\n\\n')")
            await expect(page.get_by_title("Send (Enter)", exact=True)).to_be_visible()

        await send("First message")
        await asyncio.wait_for(save_started.wait(), 5)
        # CRLF separators split between reads, and multiline SSE data.
        await page.evaluate(r"""() => {
          pushChat('data: {"choices":\r\ndata: [{"delta":{"content":"needle answer"}}]}\r');
          pushChat('\n\r'); pushChat('\n');
        }""")
        await done()
        await expect(page.locator(".msg.assistant .md")).to_contain_text("needle answer")
        assert len(saves) == 1, "Final save overtook the blocked initial save"
        release_save.set()
        await expect(page.locator(".chat-save")).to_have_text("Saved")
        first_id = next(iter(saved))
        assert saved[first_id]["messages"][-1]["content"] == "needle answer"
        assert not saved[first_id]["messages"][-1].get("stopped")

        # A cut-off stream must retain its content and expose an error.
        fail_save = True
        await send("Second message")
        await chunk("Partial answer")
        await page.evaluate("chatStream.close()")
        await expect(page.locator(".msg-error")).to_contain_text("ended before completion")
        await expect(page.get_by_role("button", name="Retry save", exact=True)).to_be_visible()
        await page.get_by_title("New chat", exact=True).click()
        await page.locator(".chat-item").filter(has=page.get_by_text("First message", exact=True)).click()
        await expect(page.locator(".msg-error")).to_contain_text("ended before completion")
        await expect(page.locator(".msg.assistant .md").last).to_contain_text("Partial answer")
        fail_save = False
        await page.get_by_role("button", name="Retry save", exact=True).click()
        await expect(page.locator(".chat-save")).to_have_text("Saved")
        assert saved[first_id]["messages"][-1]["content"] == "Partial answer"
        assert "ended before completion" in saved[first_id]["messages"][-1]["error"]

        # Export the in-memory conversation without transient UI state or keys.
        for format, label in [("json", "Export JSON"), ("md", "Export Markdown")]:
            await page.get_by_title("Export chat", exact=True).click()
            async with page.expect_download() as download_info:
                await page.get_by_role("button", name=label, exact=True).click()
            download = await download_info.value
            assert download.suggested_filename.endswith("." + format)
            content = Path(await download.path()).read_text()
            assert "needle answer" in content and "Partial answer" in content
            assert "_saveState" not in content and "server.key" not in content
            if format == "json":
                assert len(json.loads(content)["messages"]) == 4

        search = page.get_by_role("searchbox", name="Search conversations")
        await search.fill("old")
        await asyncio.wait_for(old_search.wait(), 5)
        await search.fill("needle")
        await expect(page.locator(".chat-item")).to_have_count(1)
        await search.fill("missing")
        await expect(page.get_by_text("No conversations match.", exact=True)).to_be_visible()
        fail_history = True
        await search.fill("needle")
        await expect(page.get_by_text("History is offline", exact=True)).to_be_visible()
        fail_history = False
        await page.locator(".chat-list").get_by_role("button", name="Try again", exact=True).click()
        await expect(page.locator(".chat-item")).to_have_count(1)
        await search.fill("")

        # A reply saved after navigation belongs to its original chat.
        block_save = True
        save_started.clear()
        release_save.clear()
        await send("Third message")
        await asyncio.wait_for(save_started.wait(), 5)
        await chunk("Kept after navigation")
        await page.get_by_title("New chat", exact=True).click()
        await expect(page.locator(".chat-title")).to_have_text("New chat")
        release_save.set()
        await expect(page.get_by_title("Send (Enter)", exact=True)).to_be_visible()
        for _ in range(100):
            if saved[first_id]["messages"][-1].get("stopped") and saved[first_id]["messages"][-1]["content"] == "Kept after navigation":
                break
            await asyncio.sleep(.02)
        assert saved[first_id]["messages"][-1]["content"] == "Kept after navigation"
        assert saved[first_id]["messages"][-1]["stopped"]
        assert await page.evaluate("JSON.parse(localStorage.getItem('studio.chatCurrent'))") is None

        # Failed deletion leaves history reachable; successful deletion cannot
        # be undone by a late stream finalizer or an in-flight PUT.
        await send("Delete this conversation")
        await expect(page.locator(".chat-save")).to_have_text("Saved")
        await chunk("Delete partial")
        for _ in range(200):
            active = next((conv for conv in saved.values() if conv["title"] == "Delete this conversation"), None)
            if active and active["messages"][-1]["content"] == "Delete partial":
                break
            await asyncio.sleep(.02)
        assert active["messages"][-1]["content"] == "Delete partial", "Partial reply was not autosaved"
        assert active["messages"][-1]["stopped"], "Restoring an unfinished reply must mark it stopped"
        row = page.locator(".chat-item").filter(has=page.get_by_text("Delete this conversation", exact=True))
        await row.hover()
        fail_delete = True
        await row.get_by_title("Delete", exact=True).click()
        await row.get_by_title("Click again to delete", exact=True).click()
        await expect(page.locator("#toast")).to_have_text("Cannot delete right now")
        await expect(row).to_be_visible()
        await expect(page.locator(".chat-save")).to_have_text("Saved")
        fail_delete = False
        await row.hover()
        await row.get_by_title("Delete", exact=True).click()
        await row.get_by_title("Click again to delete", exact=True).click()
        await expect(row).to_have_count(0)
        await expect(page.locator(".chat-title")).to_have_text("New chat")
        assert all(cid not in saved for cid in deleted)

        await send("Reopen this stream")
        await expect(page.locator(".chat-save")).to_have_text("Saved")
        await chunk("Newest partial before reopening")
        await page.locator(".chat-item").filter(has=page.get_by_text("Reopen this stream", exact=True)).click()
        await expect(page.locator(".msg.assistant .md")).to_contain_text("Newest partial before reopening")
        await expect(page.locator(".msg-meta")).to_contain_text("Stopped")

        for width in (390, 768, 1440):
            await page.set_viewport_size({"width": width, "height": 844})
            assert await page.evaluate("document.documentElement.scrollWidth <= innerWidth"), f"chat overflow at {width}"
        await page.screenshot(path="/tmp/fornax-chat-iteration.png", full_page=True)
        assert not errors, errors
        print("Chat browser checks passed: ordered saves, interrupted streams, retry, JSON/Markdown export, search recovery, navigation, deletion, responsive widths.")
        await browser.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True)
    parser.add_argument("--home", type=Path, required=True)
    parser.add_argument("--chromium", default="/usr/bin/chromium")
    args = parser.parse_args()
    if urlparse(args.url).hostname not in ("127.0.0.1", "localhost"):
        parser.error("use a local studio with an isolated FORNAX_HOME")
    asyncio.run(check(args.url.rstrip("/"), args.home, args.chromium))
