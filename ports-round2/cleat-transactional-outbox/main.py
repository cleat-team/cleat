"""
Transactional Outbox demo — Cleat port.

Run with:

    export DATABASE_URL="postgresql+psycopg://postgres@localhost:5432/transactional_outbox"
    uv run python main.py

Or for SQLite development:

    export DATABASE_URL="sqlite:///outbox.db"
    uv run python main.py
"""

from app import main

if __name__ == "__main__":
    main()
