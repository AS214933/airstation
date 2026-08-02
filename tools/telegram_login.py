#!/usr/bin/env python3
"""
Generate or manage a Pyrogram session string for the Airstation Telegram voice streamer.

Interactive usage:
    python tools/telegram_login.py <api_id> <api_hash>

Programmatic usage:
    python tools/telegram_login.py send-code <api_id> <api_hash> <phone>
    python tools/telegram_login.py sign-in <api_id> <api_hash> <phone> <phone_code_hash> <code> [--password <password>]
    python tools/telegram_login.py check-password <api_id> <api_hash> <phone> --password <password>
"""
import argparse
import asyncio
import json
import os
import sys

from pyrogram import Client
from pyrogram import errors


SESSION_NAME = "airstation_telegram_login"


def _login_workdir():
    return os.path.join(os.getcwd(), "storage", "telegram", "login")


def _client(api_id: int, api_hash: str):
    workdir = _login_workdir()
    os.makedirs(workdir, exist_ok=True)
    return Client(
        name=SESSION_NAME,
        api_id=api_id,
        api_hash=api_hash,
        workdir=workdir,
        in_memory=False,
    )


def _cleanup_session_file():
    session_path = os.path.join(_login_workdir(), f"{SESSION_NAME}.session")
    if os.path.exists(session_path):
        os.remove(session_path)


def _print_result(result: dict):
    print(json.dumps(result))
    sys.stdout.flush()


async def send_code(api_id: int, api_hash: str, phone: str):
    # Start with a clean session file so the code is bound to a fresh auth key.
    workdir = _login_workdir()
    session_path = os.path.join(workdir, f"{SESSION_NAME}.session")
    if os.path.exists(session_path):
        os.remove(session_path)

    client = _client(api_id, api_hash)
    await client.connect()
    try:
        sent = await client.send_code(phone)
        result = {"phoneCodeHash": sent.phone_code_hash}
        if sent.type:
            try:
                result["type"] = sent.type.__class__.__name__
            except Exception:
                pass
        _print_result(result)
    finally:
        await client.disconnect()


async def sign_in(
    api_id: int,
    api_hash: str,
    phone: str,
    phone_code_hash: str,
    code: str,
    password: str | None,
):
    client = _client(api_id, api_hash)
    await client.connect()
    try:
        if await client.is_user_authorized():
            _print_result({"sessionString": await client.export_session_string()})
            return

        try:
            await client.sign_in(phone, phone_code_hash, code)
        except errors.SessionPasswordNeeded:
            if not password:
                _print_result({"needsPassword": True})
                return
            await client.check_password(password)
        except errors.PhoneCodeInvalid:
            # The code may already have been consumed in a previous attempt.
            # If a password is available, try completing the password step.
            if password:
                await client.check_password(password)
            else:
                raise

        _print_result({"sessionString": await client.export_session_string()})
        _cleanup_session_file()
    finally:
        await client.disconnect()


async def check_password(api_id: int, api_hash: str, phone: str, password: str):
    client = _client(api_id, api_hash)
    await client.connect()
    try:
        if not await client.is_user_authorized():
            await client.check_password(password)
        _print_result({"sessionString": await client.export_session_string()})
        _cleanup_session_file()
    finally:
        await client.disconnect()


async def interactive_login(api_id: int, api_hash: str):
    client = _client(api_id, api_hash)
    await client.connect()
    try:
        if not await client.is_user_authorized():
            phone = input("Enter your phone number (with country code, e.g. +8613800138000): ")
            sent_code = await client.send_code(phone)
            code = input("Enter the Telegram login code: ")
            try:
                await client.sign_in(phone, sent_code.phone_code_hash, code)
            except errors.SessionPasswordNeeded:
                password = input("Enter your 2FA password: ")
                await client.check_password(password)

        session_string = await client.export_session_string()
        print("\nYour session string (keep it secret):\n")
        print(session_string)
        print("\nThe session string has been printed above. In normal usage, use Studio -> Settings -> Telegram voice stream to log in via the WebUI.")
    finally:
        await client.disconnect()


def main():
    # Backward-compatible interactive mode: "python telegram_login.py <api_id> <api_hash>"
    if len(sys.argv) == 3 and sys.argv[1].lstrip("-").isdigit():
        asyncio.run(interactive_login(int(sys.argv[1]), sys.argv[2]))
        return

    parser = argparse.ArgumentParser(
        description="Generate Telegram session string for Airstation",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    sc = subparsers.add_parser("send-code", help="request a login code")
    sc.add_argument("api_id", type=int)
    sc.add_argument("api_hash")
    sc.add_argument("phone")

    si = subparsers.add_parser("sign-in", help="sign in and export a session string")
    si.add_argument("api_id", type=int)
    si.add_argument("api_hash")
    si.add_argument("phone")
    si.add_argument("phone_code_hash")
    si.add_argument("code")
    si.add_argument("--password", default="")

    cp = subparsers.add_parser("check-password", help="finish login with 2FA password")
    cp.add_argument("api_id", type=int)
    cp.add_argument("api_hash")
    cp.add_argument("phone")
    cp.add_argument("--password", required=True)

    args = parser.parse_args()

    if args.command == "send-code":
        asyncio.run(send_code(args.api_id, args.api_hash, args.phone))
    elif args.command == "sign-in":
        asyncio.run(
            sign_in(
                args.api_id,
                args.api_hash,
                args.phone,
                args.phone_code_hash,
                args.code,
                args.password or None,
            )
        )
    elif args.command == "check-password":
        asyncio.run(check_password(args.api_id, args.api_hash, args.phone, args.password))


if __name__ == "__main__":
    main()
