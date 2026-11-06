// Track sources. The API takes names when setting permissions, but reports enum numbers when reading them,
// because the permission object is a protobuf struct encoded with Go's encoding/json.
export const TRACK_SOURCES = [
  { value: "camera", number: 1, label: "Camera" },
  { value: "microphone", number: 2, label: "Microphone" },
  { value: "screen_share", number: 3, label: "Screen share" },
  { value: "screen_share_audio", number: 4, label: "Screen share audio" },
] as const;

export function sourceNames(numbers: number[] | undefined): string[] {
  return (numbers ?? []).flatMap((n) => TRACK_SOURCES.filter((s) => s.number === n).map((s) => s.value));
}

export function sourceLabel(source: string): string {
  return TRACK_SOURCES.find((s) => s.value === source.toLowerCase())?.label ?? source.toLowerCase().replace(/_/g, " ");
}
