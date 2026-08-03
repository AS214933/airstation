# Installation

To run Airstation on your machine, there are two ways: using [Docker](https://docs.docker.com/) (recommended) or building it yourself using the [Go](https://go.dev/) compiler for server, [Node.js](https://nodejs.org/) with [npm](https://www.npmjs.com/) for web clients and also have [FFmpeg](https://ffmpeg.org/) installed on your system.

The player uses AMLL packages (`@applemusic-like-lyrics/core` and `@applemusic-like-lyrics/lyric`) for Apple Music-like lyrics. These are installed by `npm ci`/`npm install` with the rest of the player dependencies.

## Docker

1.  Clone Airstation repository

    ```sh
    git clone https://github.com/cheatsnake/airstation.git
    ```

    ```sh
    cd ./airstation
    ```

2.  Setup environment variables

    Next you need an `.env` file with secret keys

    ```sh
    touch .env
    ```

    Inside this file you must define 2 variables:

    ```
    AIRSTATION_SECRET_KEY=
    AIRSTATION_JWT_SIGN=
    ```

    > `AIRSTATION_SECRET_KEY` - the secret key you need to log in to the station control panel <br> `AIRSTATION_JWT_SIGN` - the key to sign the JWT session

    > Use [random string generator](https://it-tools.tech/token-generator?length=20) with a length of at least 10 characters for these variables!

3.  Build a docker image and start a new container

    ```sh
    docker compose up -d
    ```

And finally you can see:

- Control panel on [http://localhost:7331/studio/](http://localhost:7331/studio/) (extra slash matters!)
- Radio player on [http://localhost:7331](http://localhost:7331)

After logging into the control panel, open the NetEase Cloud source section and save:

- a NetEase Cloud Music playlist link, for example `https://music.163.com/#/playlist?id=3778678`
- the requested quality (`standard`, `higher`, `exhigh`, `lossless`, or `hires`)
- optionally, a NetEase login cookie such as `MUSIC_U=...` when the playlist or songs require your account

The server keeps these values in SQLite and automatically builds the public HLS stream from random playable songs in that playlist.
When lyrics are available, the player prefers NetEase word-by-word lyrics, then synced scrolling lyrics, then plain lyric text.
For overseas deployments, Airstation sends NetEase API requests with a mainland `X-Real-IP` header by default and rewrites NetEase audio CDN links from `m*.music.126.net` to `m*c.music.126.net`, matching the lightweight browser-extension unlock approach used by NetEaseMusicWorld++.
Set `AIRSTATION_NETEASE_REAL_IP` to another mainland IP if needed, or set it to `off` to disable the header.
Temporary HLS files in `AIRSTATION_TMP_DIR` are cleaned automatically after 24 hours by default.
Set `AIRSTATION_TMP_RETENTION` to another Go duration such as `12h` or `48h` if you need a different retention window.
Logs are `Info` level by default. Set `AIRSTATION_DEBUG=true` temporarily to include diagnostic Telegram ICE, media, and FFmpeg messages; remove it or set it to `false` for normal operation.

### Telegram voice stream (optional)

You can stream the current Airstation playback into one or more Telegram group/channel voice chats. Airstation reads its own local HLS playlist and transcodes it for Telegram, so Telegram does not make a second request to the NetEase audio CDN. No public HLS URL is required. This requires:

- Your `api_id` and `api_hash` from [my.telegram.org](https://my.telegram.org).
- A Telegram **user account**. Bot accounts cannot join voice chats, so Bot API tokens are not supported.

Steps:

1. Get your `api_id` and `api_hash` from [my.telegram.org](https://my.telegram.org).
2. In the Studio settings, open **Telegram voice stream** and fill in:
   - API ID
   - API hash
   - Comma-separated Telegram chat IDs (e.g. `-1001234567890`)
3. Click **Login with Telegram**. Enter the phone number (with country code, e.g. `+8613800138000`), request a code, enter the code from Telegram, and provide the 2FA password if the account has two-step verification enabled. Airstation saves the authenticated MTProto session automatically.
4. Click **Save**, then **Test credentials**. Airstation joins the configured voice chats with the user account and streams the current playback source.

> The Telegram user account must already be a member of the target group or channel, and the group/channel must have an active voice/video chat. The configuration is hot-reloaded: saving new settings restarts the streamer without restarting Airstation.

To stop the container, just type:

```sh
docker compose down
```

### Docker Compose

You can get a pre-built image from [GitHub Container Registry](https://github.com/orgs/AS214933/packages/container/package/airstations) and run it quickly with custom `docker-compose.yml` file as shown below:

```yml
# docker-compose.yml
services:
  airstation:
    image: ghcr.io/as214933/airstations:latest
    ports:
      - "7331:7331"
    volumes:
      - airstation-data:/app/storage
    restart: unless-stopped
    environment:
      AIRSTATION_SECRET_KEY: ${AIRSTATION_SECRET_KEY:-PASTE_YOUR_OWN_KEY}
      AIRSTATION_JWT_SIGN: ${AIRSTATION_JWT_SIGN:-PASTE_RANDOM_STRING}
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:7331/"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 10s

volumes:
  airstation-data:
```

> Don't forget to modify environment variables inside this file or via your own `.env` file in the same directory as the `docker-compose.yml`

## Build from source

1. Follow steps 1 and 2 from the previous section

2. Install dependencies

```sh
npm ci --prefix ./web/player
```

```sh
npm ci --prefix ./web/studio
```

If you plan to use Telegram voice streaming, make sure FFmpeg is installed and that it is built with libopus (the Debian/Ubuntu package `ffmpeg` satisfies this).

3. Build web clients

```sh
npm run build --prefix ./web/player
```

```sh
npm run build --prefix ./web/studio
```

4. Build server

```sh
go build ./cmd/main.go
```

5. Run app

```sh
./main
```

> Make sure you have [FFmpeg](https://ffmpeg.org/) installed on your system.

See the result on [http://localhost:7331](http://localhost:7331) and [http://localhost:7331/studio/](http://localhost:7331/studio/) (extra slash matters!)

## Development mode

To run the application in development mode, start each part of the application using the commands below:

```sh
npm run dev --prefix ./web/player
```

```sh
npm run dev --prefix ./web/studio
```

```sh
go run ./cmd/main.go
```
