// Formatting for figures that came from the backend. These never invent a value: a figure the server has not
// measured is shown as "not measured" by the caller, not as zero.

const compact = new Intl.NumberFormat("en", { notation: "compact", maximumFractionDigits: 1 });
const whole = new Intl.NumberFormat("en", { maximumFractionDigits: 0 });

export function formatCount(n: number): string {
  if (!Number.isFinite(n)) return "–";
  return Math.abs(n) < 10000 ? whole.format(n) : compact.format(n);
}

export function formatDecimal(n: number, digits = 1): string {
  return new Intl.NumberFormat("en", { maximumFractionDigits: digits }).format(n);
}

/** Bits per second. */
export function formatBitrate(bps: number): string {
  if (!bps || bps < 0) return "0 bps";
  if (bps < 1000) return `${Math.round(bps)} bps`;
  if (bps < 1_000_000) return `${(bps / 1000).toFixed(bps < 10_000 ? 1 : 0)} kbps`;
  if (bps < 1_000_000_000) return `${(bps / 1_000_000).toFixed(2)} Mbps`;
  return `${(bps / 1_000_000_000).toFixed(2)} Gbps`;
}

/** Short enough for a chart axis: 0, 650k, 2.6M. */
export function formatBitrateTick(bps: number): string {
  if (!bps) return "0";
  if (bps < 1000) return String(Math.round(bps));
  if (bps < 1_000_000) return `${Math.round(bps / 1000)}k`;
  return `${(bps / 1_000_000).toFixed(bps < 10_000_000 ? 1 : 0)}M`;
}

/** Short enough for a chart axis: 0, 500 MB, 1.2 GB. */
export function formatBytesTick(bytes: number): string {
  if (!bytes) return "0";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1000)), units.length - 1);
  const v = bytes / 1000 ** i;
  return `${v >= 10 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

/** Minutes on a chart axis, where small values still need their decimals. */
export function formatMinutesTick(minutes: number): string {
  if (!minutes) return "0";
  if (minutes < 10) return formatDecimal(minutes, 1);
  return formatCount(minutes);
}

/** A duration on a chart axis, with enough precision that its ticks differ. */
export function formatDurationTick(seconds: number): string {
  if (!seconds) return "0";
  if (seconds < 0.1) return `${formatDecimal(seconds, 3)}s`;
  if (seconds < 1) return `${formatDecimal(seconds, 2)}s`;
  if (seconds < 10) return `${formatDecimal(seconds, 1)}s`;
  if (seconds < 120) return `${Math.round(seconds)}s`;
  if (seconds < 7200) return `${Math.round(seconds / 60)}m`;
  return `${formatDecimal(seconds / 3600, 1)}h`;
}

/** Milliseconds on a chart axis, where small values need their decimals. */
export function formatMsTick(ms: number): string {
  return `${formatDecimal(ms, ms < 10 ? 1 : 0)} ms`;
}

export function formatBytes(bytes: number): string {
  if (!bytes || bytes < 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1000)), units.length - 1);
  const v = bytes / 1000 ** i;
  return `${v.toFixed(i === 0 || v >= 100 ? 0 : 1)} ${units[i]}`;
}

export function formatPercent(ratio: number, digits = 1): string {
  return `${(ratio * 100).toFixed(digits)}%`;
}

/** Whole seconds as 1h 02m, 3m 20s, 45s. */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "–";
  const s = Math.floor(seconds);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

export function formatMinutes(minutes: number): string {
  if (minutes < 1 && minutes > 0) return `${Math.round(minutes * 60)} sec`;
  if (minutes < 600) return `${formatDecimal(minutes, minutes < 10 ? 1 : 0)} min`;
  return `${formatDecimal(minutes / 60, 1)} h`;
}

const dateTime = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
const shortTime = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
const clock = new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" });

export function toDate(v: string | number): Date {
  // Numbers from this API are Unix seconds.
  return typeof v === "number" ? new Date(v * 1000) : new Date(v);
}

export function formatDateTime(v: string | number): string {
  return dateTime.format(toDate(v));
}

export function formatShortTime(v: string | number): string {
  return shortTime.format(toDate(v));
}

export function formatClock(v: string | number | Date): string {
  return clock.format(v instanceof Date ? v : toDate(v));
}

export function formatRelative(v: string | number, now: number = Date.now()): string {
  const diff = Math.round((toDate(v).getTime() - now) / 1000);
  const abs = Math.abs(diff);
  const suffix = diff > 0 ? "from now" : "ago";
  if (abs < 5) return "just now";
  if (abs < 60) return `${abs}s ${suffix}`;
  if (abs < 3600) return `${Math.floor(abs / 60)}m ${suffix}`;
  if (abs < 86400) return `${Math.floor(abs / 3600)}h ${suffix}`;
  return `${Math.floor(abs / 86400)}d ${suffix}`;
}

export function shortUserAgent(ua: string): string {
  if (!ua) return "Unknown client";
  const browser = /Edg\//.test(ua)
    ? "Edge"
    : /Chrome\//.test(ua) || /HeadlessChrome\//.test(ua)
      ? "Chrome"
      : /Firefox\//.test(ua)
        ? "Firefox"
        : /Safari\//.test(ua)
          ? "Safari"
          : /curl\//.test(ua)
            ? "curl"
            : ua.split(/[ /]/)[0] ?? "Unknown client";
  const os = /Windows/.test(ua) ? "Windows" : /Mac OS X/.test(ua) ? "macOS" : /Android/.test(ua) ? "Android" : /iPhone|iPad/.test(ua) ? "iOS" : /Linux/.test(ua) ? "Linux" : "";
  return os ? `${browser} on ${os}` : browser;
}
