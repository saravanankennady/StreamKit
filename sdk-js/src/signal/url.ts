import { clientProtocol, protocolVersion, version, wireCompatVersion } from '../version.js';
import { browserInfo, type BrowserInfo } from '../util/browser.js';

export interface SignalOptions {
  autoSubscribe: boolean;
  adaptiveStream?: boolean;
  /** Present when the client has an opinion on the server pausing its video under congestion. */
  subscriberAllowPause?: boolean;
  reconnect?: boolean;
  sid?: string;
  reconnectReason?: number;
  browser?: BrowserInfo;
}

/** Turns the URL an application passes into the WebSocket endpoint. Any path on it is kept, for proxies. */
export function buildRtcUrl(url: string): URL {
  const out = new URL(url);
  if (out.protocol === 'http:') out.protocol = 'ws:';
  else if (out.protocol === 'https:') out.protocol = 'wss:';
  out.pathname = `${out.pathname.replace(/\/+$/, '')}/rtc`;
  out.search = '';
  out.hash = '';
  return out;
}

export function buildSignalParams(token: string, opts: SignalOptions): URLSearchParams {
  const p = new URLSearchParams();
  p.set('access_token', token);
  p.set('auto_subscribe', opts.autoSubscribe ? '1' : '0');
  p.set('sdk', 'js');
  p.set('version', wireCompatVersion);
  p.set('streamkit_sdk', version);
  p.set('protocol', String(protocolVersion));
  p.set('client_protocol', String(clientProtocol));
  if (opts.reconnect) p.set('reconnect', '1');
  if (opts.reconnect && opts.sid) p.set('sid', opts.sid);
  if (opts.reconnect && opts.reconnectReason) p.set('reconnect_reason', String(opts.reconnectReason));
  if (opts.adaptiveStream) p.set('adaptive_stream', '1');
  if (opts.subscriberAllowPause !== undefined) p.set('subscriber_allow_pause', opts.subscriberAllowPause ? '1' : '0');
  const browser = opts.browser ?? browserInfo();
  if (browser) {
    p.set('browser', browser.name);
    p.set('browser_version', browser.version);
    if (browser.os) p.set('os', browser.os);
  }
  return p;
}

export function signalUrl(url: string, token: string, opts: SignalOptions): string {
  const out = buildRtcUrl(url);
  out.search = buildSignalParams(token, opts).toString();
  return out.toString();
}

/** The HTTP endpoint that explains why a WebSocket upgrade failed, with the same parameters. */
export function validateUrl(url: string, token: string, opts: SignalOptions): string {
  const out = buildRtcUrl(url);
  out.protocol = out.protocol === 'wss:' ? 'https:' : 'http:';
  out.pathname = `${out.pathname}/validate`;
  out.search = buildSignalParams(token, opts).toString();
  return out.toString();
}
