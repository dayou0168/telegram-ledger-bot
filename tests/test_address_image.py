from pathlib import Path
from tempfile import TemporaryDirectory

from PIL import Image

from ledger_bot.address_image import IMAGE_HEIGHT, IMAGE_WIDTH, save_address_verification_image


def test_save_address_verification_image_matches_reference_style() -> None:
    with TemporaryDirectory() as tmp:
        db_path = Path(tmp) / "ledger.db"

        url = save_address_verification_image(
            db_path=db_path,
            public_base_url="https://bot.example.com",
            address="TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ",
        )

        assert url.startswith("https://bot.example.com/uploads/address_check_")
        image_path = Path(tmp) / "uploads" / url.rsplit("/", 1)[-1]
        assert image_path.exists()

        image = Image.open(image_path)
        assert image.size == (IMAGE_WIDTH, IMAGE_HEIGHT)
        assert image.getpixel((10, 10)) == (3, 167, 123)
        orange = image.getpixel((40, 250))
        assert orange[0] > 180 and orange[1] > 70 and orange[2] < 40
