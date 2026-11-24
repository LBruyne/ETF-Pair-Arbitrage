#!/usr/bin/env python3
"""Full-page screenshot of a board as the anonymous viewer, for reviewing the
layout without a desktop. Needs a Python with playwright:

    /home/ubuntu/dashboard-run/pw-venv/bin/python3 dashboard/deploy/screenshot.py URL OUT.png [width] [light|dark]
"""
import sys, time
from playwright.sync_api import sync_playwright

url, out = sys.argv[1], sys.argv[2]
width = int(sys.argv[3]) if len(sys.argv) > 3 else 1680
theme = sys.argv[4] if len(sys.argv) > 4 else "dark"
url += ("&" if "?" in url else "?") + "theme=" + theme
with sync_playwright() as p:
    browser = p.chromium.launch()
    # A viewport taller than the page (Combo Spread is about 4200 px), so every panel is on screen and renders
    # before the capture, and kiosk mode's fixed footer sits below the last panel.
    page = browser.new_page(viewport={"width": width, "height": 4800}, device_scale_factor=1)
    page.goto(url, wait_until="networkidle", timeout=120000)
    time.sleep(5)
    # Scroll through so lazily rendered panels draw, then capture the whole page.
    height = page.evaluate("document.scrollingElement.scrollHeight")
    for _ in range(0, height, 800):
        page.mouse.wheel(0, 800)
        time.sleep(0.5)
    page.mouse.wheel(0, -height)
    time.sleep(2)
    page.screenshot(path=out, full_page=True)
    print("wrote", out, "height", height)
    browser.close()
