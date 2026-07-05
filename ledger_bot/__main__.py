from .bot import LedgerBot
from .bill_web import start_bill_web_server
from .config import load_config


def main() -> None:
    config = load_config()
    start_bill_web_server(config)
    LedgerBot(config).run_forever()


if __name__ == "__main__":
    main()
