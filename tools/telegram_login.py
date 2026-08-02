#!/usr/bin/env python3
"""
Generate a Pyrogram session string for the Airstation Telegram voice streamer.

Run this script once on the Airstation host, then paste the printed session string
into the Studio settings.

Usage:
    python tools/telegram_login.py <api_id> <api_hash>
"""
import argparse
import asyncio
import os

from pyrogram import Client


async def main(api_id: int, api_hash: str):
    workdir = os.path.join(os.getcwd(), "storage", "telegram")
    os.makedirs(workdir, exist_ok=True)

    client = Client(
        name="airstation_telegram_streamer",
        api_id=api_id,
        api_hash=api_hash,
        workdir=workdir,
        in_memory=True,
    )
    await client.connect()
    if not await client.is_user_authorized():
        phone = input("Enter your phone number (with country code, e.g. +8613800138000): ")
        sent_code = await client.send_code(phone)
        code = input("Enter the Telegram login code: ")
        try:
            await client.sign_in(phone, sent_code.phone_code_hash, code)
        except Exception:
            password = input("Enter your 2FA password (or leave empty): ")
            if password:
                await client.check_password(password)
            else:
                raise

    session_string = await client.export_session_string()
    print("\nYour session string (keep it secret):\n")
    print(session_string)
    print("\nPaste it into Studio -> Settings -> Telegram voice stream.")
    await client.disconnect()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Generate Telegram session string")
    parser.add_argument("api_id", type=int, help="Telegram API ID from my.telegram.org")
    parser.add_argument("api_hash", help="Telegram API Hash from my.telegram.org")
    args = parser.parse_args()
    asyncio.run(main(args.api_id, args.api_hash))
