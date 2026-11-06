import { describe, expect, it } from 'vitest';
import { signalUrl, validateUrl } from '../src/signal/url.js';
import { parseUserAgent } from '../src/util/browser.js';

const browser = { name: 'Chrome', version: '120.0', os: 'linux' };

describe('signalUrl', () => {
  it('turns http into ws and keeps the path', () => {
    expect(signalUrl('http://h:7880', 't', { autoSubscribe: true, browser })).toMatch(/^ws:\/\/h:7880\/rtc\?/);
    expect(signalUrl('https://h/prefix/', 't', { autoSubscribe: true, browser })).toMatch(/^wss:\/\/h\/prefix\/rtc\?/);
    expect(signalUrl('ws://h', 't', { autoSubscribe: true, browser })).toMatch(/^ws:\/\/h\/rtc\?/);
  });

  it('never goes near the v1 path', () => {
    expect(signalUrl('ws://h', 't', { autoSubscribe: true, browser })).not.toContain('/rtc/v1');
  });

  it('sends what a server needs on a join', () => {
    const p = new URL(signalUrl('ws://h', 'tok', { autoSubscribe: false, adaptiveStream: true, browser })).searchParams;
    expect(p.get('access_token')).toBe('tok');
    expect(p.get('auto_subscribe')).toBe('0');
    expect(p.get('sdk')).toBe('js');
    expect(p.get('protocol')).toBe('17');
    expect(p.get('client_protocol')).toBe('2');
    expect(p.get('adaptive_stream')).toBe('1');
    expect(p.get('browser')).toBe('Chrome');
    expect(p.has('reconnect')).toBe(false);
    expect(p.has('sid')).toBe(false);
  });

  it('reports a version a server takes as able to resume', () => {
    // A server treats a JS client below 1.6.3 as unable to read the reconnect response.
    const v = new URL(signalUrl('ws://h', 't', { autoSubscribe: true, browser })).searchParams.get('version')!;
    const [major, minor] = v.split('.').map(Number);
    expect(major > 1 || (major === 1 && minor >= 7)).toBe(true);
  });

  it('adds the session on a resume', () => {
    const p = new URL(signalUrl('ws://h', 't', { autoSubscribe: true, reconnect: true, sid: 'PA_1', reconnectReason: 1, browser })).searchParams;
    expect(p.get('reconnect')).toBe('1');
    expect(p.get('sid')).toBe('PA_1');
    expect(p.get('reconnect_reason')).toBe('1');
  });

  it('leaves out a resume reason of zero, and a session ID outside a resume', () => {
    const p = new URL(signalUrl('ws://h', 't', { autoSubscribe: true, reconnect: true, sid: 'PA_1', reconnectReason: 0, browser })).searchParams;
    expect(p.has('reconnect_reason')).toBe(false);
    expect(new URL(signalUrl('ws://h', 't', { autoSubscribe: true, sid: 'PA_1', browser })).searchParams.has('sid')).toBe(false);
  });
});

describe('validateUrl', () => {
  it('goes over HTTP, with the same parameters', () => {
    const u = new URL(validateUrl('wss://h/prefix', 'tok', { autoSubscribe: true, browser }));
    expect(u.protocol).toBe('https:');
    expect(u.pathname).toBe('/prefix/rtc/validate');
    expect(u.searchParams.get('access_token')).toBe('tok');
    expect(new URL(validateUrl('ws://h', 't', { autoSubscribe: true, browser })).protocol).toBe('http:');
  });
});

describe('parseUserAgent', () => {
  it('tells browsers apart', () => {
    const chrome = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36';
    const firefox = 'Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0';
    const safari = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15';
    const edge = 'Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36 Edg/120.0.1';
    expect(parseUserAgent(chrome)).toEqual({ name: 'Chrome', version: '156.0.0.0', os: 'linux' });
    expect(parseUserAgent(firefox)?.name).toBe('Firefox');
    expect(parseUserAgent(safari)).toEqual({ name: 'Safari', version: '17.0', os: 'macos' });
    expect(parseUserAgent(edge)?.name).toBe('Edge');
    expect(parseUserAgent('curl/8')).toBeUndefined();
  });
});
