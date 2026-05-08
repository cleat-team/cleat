from cleat_sdk import cleat_entry


@cleat_entry
def workflow() -> None:
    f = open("file.txt")
    f.close()
