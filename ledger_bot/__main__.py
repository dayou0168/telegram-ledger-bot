from .bot import LedgerBot
from .config import load_config


def main() -> None:
    LedgerBot(load_config()).run_forever()


if __name__ == "__main__":
    main()

