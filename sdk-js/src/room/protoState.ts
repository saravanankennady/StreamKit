import { StreamState } from '../enums.js';
import { StreamState as ProtoStreamState } from '../proto/livekit_rtc_pb.js';

export function streamStateFromProto(state: ProtoStreamState): StreamState {
  switch (state) {
    case ProtoStreamState.ACTIVE:
      return StreamState.Active;
    case ProtoStreamState.PAUSED:
      return StreamState.Paused;
    default:
      return StreamState.Unknown;
  }
}
