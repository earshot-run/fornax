"""Optional Chromium regressions for the studio; requires Python Playwright.

Run a freshly built studio with an isolated FORNAX_HOME, then pass --url and
--home. The real embedded frontend is exercised with deterministic API fixtures;
Go tests separately exercise the server and upstream discovery behavior.
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
        context = await browser.new_context(viewport={"width": 1440, "height": 1000}, color_scheme="light")
        await context.add_cookies([{
            "name": "fornax_studio", "value": (home / "server.key").read_text().strip(),
            "url": url, "httpOnly": True,
        }])
        page = await context.new_page()
        errors, requests = [], []
        page.on("pageerror", lambda e: errors.append(str(e)))
        models = []
        first_search = asyncio.Event()
        history_requested = asyncio.Event()
        restore_history = False
        fail_popular = False
        download_attempts = 0
        # Hold SSE steady while each test explicitly supplies a server snapshot.
        await page.add_init_script("""
            window.EventSource = class {
              static CLOSED = 2;
              constructor() { window.testStudioEvents = this; this.readyState = 1; }
              close() {}
            };
        """)
        picks = [
            {"key": "small", "title": "Small model", "kind": "chat", "ref": "hf:org/Small/a.gguf", "bytes": 10**9, "fit": "fits", "blurb": "A compact starter."},
            {"key": "large", "title": "Large model", "kind": "chat", "ref": "hf:org/Large/b.gguf", "bytes": 20*10**9, "fit": "won't fit", "blurb": "Needs more memory.", "readiness": {"supported": False, "installed": False, "reason": "No compatible engine for this platform."}},
        ]

        async def api(route):
            nonlocal download_attempts
            request = route.request
            path = urlparse(request.url).path
            q = parse_qs(urlparse(request.url).query)
            body = request.post_data_json if request.post_data else None
            requests.append((path, q, body))
            status, response = 200, None
            if path == "/api/models":
                response = models
            elif path == "/api/hub/picks":
                response = {"memory": 8*1024**3, "picks": picks}
            elif path == "/api/hub/popular":
                if fail_popular:
                    status, response = 502, "Discovery is offline"
                else:
                    response = [{"repo": "org/Recent", "downloads": 42, "ggufs": ["recent-Q4_K_M.gguf", "recent-Q8_0-00001-of-00002.gguf", "recent-Q8_0-00002-of-00002.gguf", "mmproj.gguf"], "updated": "2026-10-01T00:00:00Z"}]
            elif path == "/api/hub/preview":
                selected = q.get("file", [""])[0]
                ref = q.get("ref", ["hf:org/Small/a.gguf"])[0]
                if not selected:
                    selected = "a.gguf" if q.get("key") else ref.split("/", 2)[2] if ref.count("/") >= 2 else "recent-Q4_K_M.gguf"
                response = {"fit": "fits", "preview": {"ref": ref if ref.count("/") >= 2 else ref+"/"+selected, "file": selected, "kind": "text", "bytes": 1500000000,
                    "readiness": {"supported": True, "engine": "llama.cpp", "backend": "cpu", "installed": False, "reason": "Compatible engine build available; downloaded on first use."},
                    "files": [{"ref": "hf:org/Files/"+selected, "file": selected, "role": "weights", "bytes": 1000000000},
                        {"ref": "hf:org/Files/mmproj.gguf", "file": "mmproj.gguf", "role": "projector", "flag": "mmproj", "bytes": 500000000}], "args": ["--steps", "4"]}}
            elif path == "/api/hub/search":
                query = q["q"][0]
                if query == "old":
                    first_search.set()
                    await asyncio.sleep(.25)
                if query == "offline":
                    status, response = 502, "Search is offline"
                else:
                    response = [{"repo": "org/"+query, "downloads": 1, "ggufs": ["a.gguf"]}]
            elif path == "/api/hub/downloads" and request.method == "POST":
                download_attempts += 1
                if download_attempts == 1:
                    status, response = 503, "Please try again"
                else:
                    response = {"id": "download1", "key": body.get("key", ""), "ref": body.get("ref", "hf:org/Small/a.gguf"), "kind": body.get("kind", ""), "title": "Small model", "state": "running", "done": 0, "total": 0}
            elif path.startswith("/api/hub/downloads/"):
                status, response = 204, ""
            elif path == "/api/chat/load":
                response = {"state": "ready", "model": body["model"]}
            elif path == "/api/chat/status":
                response = {"state": ""}
            elif path == "/api/chats":
                await asyncio.sleep(.2)
                response = [{"id": "saved", "title": "Previous conversation", "model": "chat-1", "updated": "2026-10-01T00:00:00Z", "count": 1}] if restore_history else []
            elif path == "/api/chats/saved":
                history_requested.set()
                await asyncio.sleep(.5)
                response = {"id": "saved", "title": "Previous conversation", "model": "chat-1", "messages": [{"role": "user", "content": "Hello"}]}
            else:
                await route.continue_()
                return
            try:
                await route.fulfill(status=status, content_type="application/json" if not isinstance(response, str) else "text/plain", body=json.dumps(response) if not isinstance(response, str) else response)
            except Exception:
                if not (path == "/api/hub/search" and q.get("q") == ["old"]):
                    raise

        await page.route("**/api/**", api)
        await page.goto(url + "/#models")
        await expect(page.get_by_role("heading", name="Small model", exact=True)).to_be_visible()
        large = page.locator(".pick-card").filter(has=page.get_by_role("heading", name="Large model", exact=True))
        await expect(large.get_by_role("button", name="Unavailable here", exact=True)).to_be_disabled()
        await expect(large.get_by_text("No compatible engine for this platform.", exact=True)).to_be_visible()
        await page.get_by_label("Only picks that fit").check()
        await expect(page.get_by_role("heading", name="Large model", exact=True)).to_have_count(0)
        await page.get_by_label("Only picks that fit").uncheck()
        await expect(page.get_by_role("heading", name="Large model", exact=True)).to_be_visible()

        theme = page.locator("#theme-toggle")
        await theme.click()  # system -> light
        await theme.click()  # light -> dark
        await page.reload()
        assert await page.evaluate("getComputedStyle(document.documentElement).colorScheme") == "dark"
        await theme.click()  # dark -> system
        assert await page.evaluate("getComputedStyle(document.documentElement).colorScheme") == "light"
        await page.emulate_media(color_scheme="dark")
        assert await page.evaluate("getComputedStyle(document.documentElement).colorScheme") == "dark"
        await theme.click()  # explicit light overrides a dark OS
        assert await page.evaluate("getComputedStyle(document.documentElement).colorScheme") == "light"

        await page.get_by_label("Sort models").select_option("lastModified")
        await expect(page.get_by_role("heading", name="Recently updated on Hugging Face")).to_be_visible()
        assert any(path == "/api/hub/popular" and q.get("sort") == ["lastModified"] for path, q, _ in requests)
        search = page.get_by_role("searchbox", name="Search models")
        await search.fill("old")
        await asyncio.wait_for(first_search.wait(), 5)
        await search.fill("new")  # invalidate old response before the new debounce fires
        await page.wait_for_timeout(300)  # old would finish while the replacement is still debouncing
        await expect(page.get_by_text("org/old", exact=True)).to_have_count(0)
        await expect(page.get_by_text("org/new", exact=True)).to_be_visible()
        await search.fill("")
        await expect(page.get_by_text("org/new", exact=True)).not_to_be_visible()
        await search.fill("offline")
        await expect(page.get_by_text("Search is offline", exact=True)).to_be_visible()
        await expect(page.get_by_text("No GGUF models match", exact=False)).to_have_count(0)
        await search.fill("")

        card = page.locator(".pick-card").filter(has=page.get_by_role("heading", name="Small model", exact=True))
        await card.get_by_role("button", name="Inspect files", exact=True).click()
        await expect(card.get_by_text("1.4 GB total model files", exact=True)).to_be_visible()
        await expect(card.get_by_text("mmproj: mmproj.gguf", exact=True)).to_be_visible()
        await expect(card.get_by_text("Engine downloads and runtime memory overhead are additional.", exact=True)).to_be_visible()
        await card.get_by_role("button", name="Download", exact=True).click()
        await expect(page.locator("#toast")).to_have_text("Please try again")
        await expect(card.get_by_role("button", name="Download", exact=True)).to_be_enabled()
        await card.get_by_role("button", name="Download", exact=True).click()
        await expect(page.get_by_role("button", name="Cancel download", exact=True)).to_be_visible()
        await search.fill("new")
        await expect(page.get_by_role("button", name="Cancel download", exact=True)).to_be_visible()
        await page.get_by_role("button", name="Cancel download", exact=True).click()
        await page.evaluate("window.testStudioEvents.onmessage({data: JSON.stringify({jobs: [], library: -1, downloads: []})})")
        await search.fill("")
        # Failed non-chat downloads must retain their type on retry.
        await page.evaluate("window.testStudioEvents.onmessage({data: JSON.stringify({jobs: [], library: -1, downloads: [{id:'failed1',ref:'hf:org/Video',kind:'video',title:'Video retry',state:'failed',error:'Interrupted'}]})})")
        failed = page.locator(".model-row").filter(has=page.get_by_text("Video retry", exact=True))
        await failed.get_by_role("button", name="Try again", exact=True).click()
        assert any(path == "/api/hub/downloads" and body and body.get("kind") == "video" for path, _, body in requests)
        await page.evaluate("window.testStudioEvents.onmessage({data: JSON.stringify({jobs: [], library: -1, downloads: []})})")

        # Generic transfers resolve a checked reference before offering Download.
        recent = page.locator(".result-item").filter(has=page.get_by_text("org/Recent", exact=True))
        await recent.get_by_role("button", name="Check files", exact=True).click()
        await expect(recent.get_by_role("button", name="Download", exact=True)).to_be_visible()
        await expect(recent.get_by_text("1.4 GB total model files", exact=True)).to_be_visible()
        # Exact choices exclude projectors and noninitial shards and submit the checked ref.
        choice = page.get_by_label("Weight file for org/Recent", exact=True)
        await expect(choice.locator("option")).to_have_count(3)
        await choice.select_option("recent-Q8_0-00001-of-00002.gguf")
        recent = page.locator(".result-item").filter(has=page.get_by_text("org/Recent", exact=True))
        await expect(recent.get_by_text("1.4 GB total model files", exact=True)).to_be_visible()
        await recent.get_by_role("button", name="Download", exact=True).click()
        assert any(path == "/api/hub/downloads" and body and body.get("ref") == "hf:org/Recent/recent-Q8_0-00001-of-00002.gguf" for path, _, body in requests)
        await page.evaluate("window.testStudioEvents.onmessage({data: JSON.stringify({jobs: [], library: -1, downloads: [{id:'resume1',ref:'hf:org/Small/Q8_0.gguf',title:'Interrupted quant',state:'interrupted',error:'Studio stopped',request:{key:'small',file:'Q8_0.gguf'}}]})})")
        await page.get_by_role("button", name="Resume", exact=True).click()
        assert any(path == "/api/hub/downloads" and body and body.get("file") == "Q8_0.gguf" and body.get("key") == "small" for path, _, body in requests)
        await page.evaluate("window.testStudioEvents.onmessage({data: JSON.stringify({jobs: [], library: -1, downloads: []})})")

        for kind, mode in [("text", "chat"), ("image", "image"), ("speech", "voice"), ("video", "video")]:
            for n in (1, 2):
                models.append({"id": f"{mode}-{n}", "name": f"{mode.title()} {n}", "kind": kind, "chat": kind == "text", "runtime": "llama" if kind == "text" else "sd", "bytes": 10**9, "fit": "fits"})
        await page.evaluate("Studio.loadModels()")
        for mode in ("chat", "image", "voice", "video", "chat"):
            await page.goto(url + "/#models")
            row = page.locator(".model-row").filter(has=page.get_by_text(f"{mode.title()} 2", exact=True))
            await row.get_by_role("button", name="Open", exact=True).click()
            await expect(page.locator("#mode-"+mode)).to_be_visible()
            if mode == "chat":
                await expect(page.locator(".model-name")).to_have_text("Chat 2")
                await page.wait_for_timeout(300)  # delayed chat restoration cannot override selection
                await expect(page.locator(".model-name")).to_have_text("Chat 2")
            else:
                selector = "#model" if mode == "image" else "#mode-"+mode+" select"
                await expect(page.locator(selector).first).to_have_value(mode+"-2")

        restore_history = True
        await page.evaluate('localStorage.setItem("studio.chatCurrent", JSON.stringify("saved"))')
        await page.goto(url + "/#chat")
        await page.reload()
        await asyncio.wait_for(history_requested.wait(), 5)
        await page.locator('a[data-mode="models"]').click()
        row = page.locator(".model-row").filter(has=page.get_by_text("Chat 2", exact=True))
        await row.get_by_role("button", name="Open", exact=True).click()
        await page.wait_for_timeout(600)
        await expect(page.locator(".model-name")).to_have_text("Chat 2")
        await expect(page.locator(".chat-title")).to_have_text("New chat")
        restore_history = False
        fail_popular = True
        await page.goto(url + "/#models")
        await expect(page.get_by_text("Couldn't reach Hugging Face. Your installed models are still available.", exact=True)).to_be_visible()
        fail_popular = False
        await page.locator(".model-notice").get_by_role("button", name="Try again", exact=True).click()
        await expect(page.get_by_text("org/Recent", exact=True)).to_be_visible()
        for width in (390, 768, 1440):
            await page.set_viewport_size({"width": width, "height": 844})
            assert await page.evaluate("document.documentElement.scrollWidth <= innerWidth"), f"overflow at {width}"
        assert not errors, errors
        print("Studio browser regressions passed: appearance, fit filter, recency sorting, stale search, search errors, download retry/cancel/type/resume, exact file previews, readiness, model selection, offline recovery, responsive widths.")
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
