#!/usr/bin/env python3
"""
Airstation Telegram voice streamer.

Connects to Telegram as a user via Pyrogram and streams the Airstation HLS
playlist into one or more Telegram group/channel voice chats using py-tgcalls.

Configuration is read from stdin as a single line of JSON, then the process
blocks until interrupted. Logs are written to stderr.
"""
import argparse
import asyncio
import json
import logging
import os
import sys

from pyrogram import Client
from pytgcalls import PyTgCalls, idle
from pytgcalls.types import MediaStream
from pytgcalls.types.stream.audio_quality import AudioQuality
from pytgcalls.types.stream.video_quality import VideoQuality


class TelegramStreamer:
    def __init__(self, config: dict):
        self.api_id = int(config["api_id"])
        self.api_hash = config["api_hash"]
        self.session_string = config.get("session_string") or config.get("session")
        self.chat_ids = [int(c) for c in config.get("chat_ids", [])]
        self.stream_url = config.get("stream_url", "http://localhost:7331/stream")
        self.log_level = config.get("log_level", "INFO").upper()
        self.workdir = config.get("workdir", os.getcwd())

        self.app: Client | None = None
        self.call: PyTgCalls | None = None

    def setup_logging(self):
        logging.basicConfig(
            level=getattr(logging, self.log_level, logging.INFO),
            format="%(asctime)s [%(levelname)s] %(message)s",
            stream=sys.stderr,
        )

    async def run(self):
        self.setup_logging()
        if not self.chat_ids:
            logging.error("No Telegram chat IDs configured")
            sys.exit(1)

        logging.info("Starting Telegram voice streamer")
        logging.info("Target chats: %s", self.chat_ids)
        logging.info("Stream URL: %s", self.stream_url)

        os.makedirs(self.workdir, exist_ok=True)

        self.app = Client(
            name="airstation_telegram_streamer",
            api_id=self.api_id,
            api_hash=self.api_hash,
            session_string=self.session_string,
            workdir=self.workdir,
            in_memory=True,
            no_updates=True,
        )
        self.call = PyTgCalls(self.app)

        await self.call.start()
        for chat_id in self.chat_ids:
            logging.info("Joining voice chat %s", chat_id)
            await self.call.play(
                chat_id,
                MediaStream(
                    self.stream_url,
                    audio_parameters=AudioQuality.STUDIO,
                    video_parameters=VideoQuality.SD_360p,
                    video_flags=MediaStream.Flags.IGNORE,
                ),
            )

        logging.info("Streaming started")
        await idle()
        logging.info("Streamer stopped")


def main():
    parser = argparse.ArgumentParser(description="Airstation Telegram voice streamer")
    parser.add_argument(
        "config",
        nargs="?",
        default="-",
        help="Path to JSON config file, or '-' to read from stdin",
    )
    args = parser.parse_args()

    raw = sys.stdin.readline() if args.config == "-" else open(args.config).read()
    config = json.loads(raw)
    streamer = TelegramStreamer(config)
    try:
        asyncio.run(streamer.run())
    except Exception:
        logging.exception("Streamer failed")
        sys.exit(1)


if __name__ == "__main__":
    main()
