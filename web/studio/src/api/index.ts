import {
    NetEaseConfig,
    NetEasePublicConfig,
    PlaybackState,
    ResponseErr,
    ResponseOK,
    StationInfo,
    TelegramVoiceConfig,
    TelegramVoicePublicConfig,
} from "./types";
import { jsonRequestParams } from "./utils";

export const API_HOST = "";
export const API_PREFIX = "/api/v1";

class AirstationAPI {
    private host: string;
    private prefix: string;
    private url: () => string;

    constructor(host: string, prefix: string) {
        this.host = host;
        this.prefix = prefix;
        this.url = () => `${this.host + this.prefix}`;
    }

    async login(secret: string) {
        const url = `${this.url()}/login`;
        return await this.makeRequest<ResponseOK>(url, jsonRequestParams("POST", { secret }));
    }

    async getPlayback() {
        const url = `${this.url()}/playback`;
        return await this.makeRequest<PlaybackState>(url);
    }

    async pausePlayback() {
        const url = `${this.url()}/playback/pause`;
        return await this.makeRequest<PlaybackState>(url, jsonRequestParams("POST", {}));
    }

    async playPlayback() {
        const url = `${this.url()}/playback/play`;
        return await this.makeRequest<PlaybackState>(url, jsonRequestParams("POST", {}));
    }

    async getNetEaseConfig() {
        const url = `${this.url()}/netease/config`;
        return await this.makeRequest<NetEasePublicConfig>(url);
    }

    async editNetEaseConfig(config: NetEaseConfig) {
        const url = `${this.url()}/netease/config`;
        return await this.makeRequest<NetEasePublicConfig>(url, jsonRequestParams("PUT", config));
    }

    async syncNetEasePlaylist() {
        const url = `${this.url()}/netease/sync`;
        return await this.makeRequest<NetEasePublicConfig>(url, jsonRequestParams("POST", {}));
    }

    async getStationInfo() {
        const url = `${this.url()}/station/info`;
        return await this.makeRequest<StationInfo>(url);
    }

    async editStationInfo(info: StationInfo) {
        const url = `${this.url()}/station/info`;
        return await this.makeRequest<StationInfo>(url, jsonRequestParams("PUT", info));
    }


    async getTelegramVoiceConfig() {
        const url = `${this.url()}/telegram/config`;
        return await this.makeRequest<TelegramVoicePublicConfig>(url);
    }

    async editTelegramVoiceConfig(config: TelegramVoiceConfig) {
        const url = `${this.url()}/telegram/config`;
        return await this.makeRequest<TelegramVoicePublicConfig>(url, jsonRequestParams("PUT", config));
    }

    async testTelegramVoiceConfig(config: TelegramVoiceConfig) {
        const url = `${this.url()}/telegram/test`;
        return await this.makeRequest<ResponseOK>(url, jsonRequestParams("POST", config));
    }

    private async makeRequest<T>(url: string, params: RequestInit = {}): Promise<T> {
        const resp = await fetch(url, params);
        if (!resp.ok) {
            const body: ResponseErr = await resp.json();
            throw new Error(body.message);
        }

        return resp.json();
    }
}

export const airstationAPI = new AirstationAPI(API_HOST, API_PREFIX);
