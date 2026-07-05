from __future__ import annotations

import hashlib
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont


IMAGE_WIDTH = 1232
IMAGE_HEIGHT = 397
GREEN = "#03a77b"
ORANGE = "#db7000"
YELLOW = "#fff05a"
DARK = "#173333"


def save_address_verification_image(
    *,
    db_path: Path,
    public_base_url: str | None,
    address: str,
) -> str:
    directory = Path(db_path).parent / "uploads"
    directory.mkdir(parents=True, exist_ok=True)
    digest = hashlib.sha256(address.encode("utf-8")).hexdigest()
    filename = f"address_check_{digest[:24]}.png"
    render_address_image(address, directory / filename)
    base = (public_base_url or "").rstrip("/")
    if base:
        return f"{base}/uploads/{filename}"
    return f"/uploads/{filename}"


def render_address_image(address: str, path: Path) -> None:
    image = Image.new("RGB", (IMAGE_WIDTH, IMAGE_HEIGHT), GREEN)
    draw = ImageDraw.Draw(image)

    logo_font = load_latin_font(34, bold=False)
    title_font = load_cjk_font(54, bold=True)
    subtitle_font = load_cjk_font(28, bold=True)
    address_font = fit_address_font(draw, address, max_width=1136, start_size=57, min_size=38)

    draw_tron_mark(draw, 32, 48)
    draw.text((82, 53), "TRON", font=logo_font, fill="#1c3334")

    title = "USDT防篡改验证核对"
    title_box = draw.textbbox((0, 0), title, font=title_font)
    draw.text(((IMAGE_WIDTH - (title_box[2] - title_box[0])) / 2, 82), title, font=title_font, fill=YELLOW)

    subtitle = "《请双方谨慎核对地址是否与图中一致,如有误停止付款》"
    subtitle_box = draw.textbbox((0, 0), subtitle, font=subtitle_font)
    draw.text(((IMAGE_WIDTH - (subtitle_box[2] - subtitle_box[0])) / 2, 166), subtitle, font=subtitle_font, fill=DARK)

    box = (28, 232, IMAGE_WIDTH - 28, 342)
    draw.rectangle(box, fill=ORANGE, outline="#ffffff", width=1)

    address_box = draw.textbbox((0, 0), address, font=address_font)
    address_width = address_box[2] - address_box[0]
    address_height = address_box[3] - address_box[1]
    draw.text(
        ((IMAGE_WIDTH - address_width) / 2, box[1] + ((box[3] - box[1] - address_height) / 2) - 4),
        address,
        font=address_font,
        fill="#ffffff",
        stroke_width=1,
        stroke_fill="#ffffff",
    )

    image.save(path, "PNG", optimize=True)


def draw_tron_mark(draw: ImageDraw.ImageDraw, x: int, y: int) -> None:
    outer = [(x, y), (x + 42, y + 12), (x + 14, y + 48)]
    draw.polygon(outer, fill="#f0183a")
    inner = [(x + 9, y + 8), (x + 34, y + 15), (x + 15, y + 37)]
    draw.polygon(inner, fill="#ffffff")
    draw.line((x + 9, y + 8, x + 15, y + 37, x + 34, y + 15), fill="#f0183a", width=2)


def fit_address_font(
    draw: ImageDraw.ImageDraw,
    text: str,
    *,
    max_width: int,
    start_size: int,
    min_size: int,
) -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    for size in range(start_size, min_size - 1, -1):
        font = load_latin_font(size, bold=True)
        box = draw.textbbox((0, 0), text, font=font, stroke_width=1)
        if box[2] - box[0] <= max_width:
            return font
    return load_latin_font(min_size, bold=True)


def load_cjk_font(size: int, *, bold: bool = False) -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    candidates = [
        "C:/Windows/Fonts/msyhbd.ttc" if bold else "C:/Windows/Fonts/msyh.ttc",
        "C:/Windows/Fonts/simhei.ttf",
        "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc" if bold else "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
        "/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
        "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf" if bold else "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
    ]
    return load_first_font(candidates, size)


def load_latin_font(size: int, *, bold: bool = False) -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    candidates = [
        "C:/Windows/Fonts/arialbd.ttf" if bold else "C:/Windows/Fonts/arial.ttf",
        "C:/Windows/Fonts/impact.ttf" if bold else "C:/Windows/Fonts/arial.ttf",
        "/usr/share/fonts/truetype/dejavu/DejaVuSansCondensed-Bold.ttf" if bold else "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
        "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc" if bold else "/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
    ]
    return load_first_font(candidates, size)


def load_first_font(candidates: list[str], size: int) -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    for candidate in candidates:
        try:
            return ImageFont.truetype(candidate, size=size)
        except OSError:
            continue
    return ImageFont.load_default()
