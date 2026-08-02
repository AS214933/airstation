FROM node:22-alpine AS player
WORKDIR /app
COPY ./web/player/package*.json ./
RUN npm install
COPY ./web/player .
ARG AIRSTATION_PLAYER_TITLE
ENV AIRSTATION_PLAYER_TITLE=$AIRSTATION_PLAYER_TITLE
RUN npm run build

FROM node:22-alpine AS studio
WORKDIR /app
COPY ./web/studio/package*.json ./
RUN npm install
COPY ./web/studio .
RUN npm run build

FROM golang:1.26-alpine AS server
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/main ./cmd/main.go

FROM python:3.12-slim
WORKDIR /app

# Install FFmpeg and clean up apt caches.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ffmpeg && \
    rm -rf /var/lib/apt/lists/*

# Install py-tgcalls and pyrogrammod (a Pyrogram fork that py-tgcalls 2.3.3
# is actually tested against; official Pyrogram 2.x is missing symbols such as
# GroupcallForbidden/GroupcallInvalid/InputGroupCallSlug).
RUN pip install --no-cache-dir py-tgcalls==2.3.3 pyrogrammod==2.4.1 && \
    python3 -c "from pyrogram import Client; from pytgcalls import PyTgCalls; PyTgCalls(Client('x', api_id=1, api_hash='x', in_memory=True, no_updates=True)); print('py-tgcalls ok')"

COPY --from=server /app/bin/main .
COPY --from=player /app/dist ./web/player/dist
COPY --from=studio /app/dist ./web/studio/dist
COPY tools/ ./tools/

EXPOSE 7331
ENTRYPOINT ["./main"]
