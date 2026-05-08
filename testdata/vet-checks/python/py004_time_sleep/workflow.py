import time
from cleat_sdk import cleat_entry


@cleat_entry
def workflow() -> None:
    time.sleep(1)
