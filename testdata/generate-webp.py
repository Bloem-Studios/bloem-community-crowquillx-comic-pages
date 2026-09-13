"""Generate original synthetic WebP patterns; MIT-licensed artwork.

Run from any directory with Pillow and libwebp installed. The committed
fixtures were generated with Pillow 12.1.1. No source artwork is required.
"""

from pathlib import Path
from PIL import Image

fixture_dir = Path(__file__).resolve().parent / "fixtures"
rgb = Image.new("RGB", (16, 12))
rgb.putdata([
    ((x * 17) % 256, (y * 23) % 256, ((x ^ y) * 19) % 256)
    for y in range(12) for x in range(16)
])
rgb.save(fixture_dir / "pattern-lossless.webp", lossless=True, method=6)
rgb.save(fixture_dir / "pattern-lossy.webp", quality=80, method=6)
rgba = rgb.convert("RGBA")
rgba.putalpha(Image.frombytes("L", (16, 12), bytes(
    (x * 13 + y * 7) % 256 for y in range(12) for x in range(16)
)))
rgba.save(fixture_dir / "pattern-alpha.webp", quality=80, method=6)
