export interface Track {
    id: string;
    name: string;
    artist: string;
    coverURL: string;
    path: string;
    duration: number;
    bitRate: number;
}

export interface PlaybackState {
    currentTrack: Track | null;
    currentNetEaseID: number;
    currentTrackElapsed: number;
    isPlaying: boolean;
    updatedAt: number;
}

export interface ResponseErr {
    message: string;
}

export interface ResponseOK {
    message: string;
}

export type NetEaseQuality = "standard" | "higher" | "exhigh" | "lossless" | "hires";

export interface NetEaseConfig {
    playlistURL: string;
    quality: NetEaseQuality;
    cookie?: string;
    clearCookie?: boolean;
}

export interface NetEasePublicConfig {
    playlistURL: string;
    playlistID: string;
    quality: NetEaseQuality;
    hasCookie: boolean;
    accountName: string;
    trackCount: number;
    lastError: string;
    lastSyncedAt: number;
}

export interface StationInfo {
    name: string;
    description: string;
    faviconURL: string;
    logoURL: string;
    location: string;
    timezone: string;
    links: string;
    theme: string;
}

export interface TelegramVoiceConfig {
    enabled: boolean;
    apiID: number;
    apiHash: string;
    sessionString: string;
    streamURL: string;
    chatIDs: string[];
}

export interface TelegramVoicePublicConfig {
    enabled: boolean;
    streamURL: string;
    chatIDs: string[];
    hasAPIID: boolean;
    hasAPIHash: boolean;
    hasSession: boolean;
}

export interface TelegramSendCodeRequest {
    phone: string;
    apiID?: number;
    apiHash?: string;
}

export interface TelegramSendCodeResponse {
    phoneCodeHash: string;
}

export interface TelegramSignInRequest {
    phone: string;
    phoneCodeHash: string;
    code: string;
    password?: string;
    apiID?: number;
    apiHash?: string;
}

export interface TelegramSignInResponse {
    needsPassword: boolean;
}
