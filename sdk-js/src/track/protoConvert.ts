import { ConnectionQuality, TrackKind, TrackSource } from '../enums.js';
import { ConnectionQuality as ProtoQuality, TrackSource as ProtoSource, TrackType } from '../proto/livekit_models_pb.js';

export function sourceToProto(source: TrackSource): ProtoSource {
  switch (source) {
    case TrackSource.Camera:
      return ProtoSource.CAMERA;
    case TrackSource.Microphone:
      return ProtoSource.MICROPHONE;
    case TrackSource.ScreenShare:
      return ProtoSource.SCREEN_SHARE;
    case TrackSource.ScreenShareAudio:
      return ProtoSource.SCREEN_SHARE_AUDIO;
    default:
      return ProtoSource.UNKNOWN;
  }
}

export function sourceFromProto(source: ProtoSource): TrackSource {
  switch (source) {
    case ProtoSource.CAMERA:
      return TrackSource.Camera;
    case ProtoSource.MICROPHONE:
      return TrackSource.Microphone;
    case ProtoSource.SCREEN_SHARE:
      return TrackSource.ScreenShare;
    case ProtoSource.SCREEN_SHARE_AUDIO:
      return TrackSource.ScreenShareAudio;
    default:
      return TrackSource.Unknown;
  }
}

export function kindToProto(kind: TrackKind): TrackType {
  switch (kind) {
    case TrackKind.Audio:
      return TrackType.AUDIO;
    case TrackKind.Video:
      return TrackType.VIDEO;
    default:
      return TrackType.DATA;
  }
}

export function kindFromProto(type: TrackType): TrackKind | undefined {
  switch (type) {
    case TrackType.AUDIO:
      return TrackKind.Audio;
    case TrackType.VIDEO:
      return TrackKind.Video;
    default:
      return undefined;
  }
}

export function qualityFromProto(q: ProtoQuality): ConnectionQuality {
  switch (q) {
    case ProtoQuality.EXCELLENT:
      return ConnectionQuality.Excellent;
    case ProtoQuality.GOOD:
      return ConnectionQuality.Good;
    case ProtoQuality.POOR:
      return ConnectionQuality.Poor;
    case ProtoQuality.LOST:
      return ConnectionQuality.Lost;
    default:
      return ConnectionQuality.Unknown;
  }
}
