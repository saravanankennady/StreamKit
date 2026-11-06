export interface BrowserInfo {
  name: string;
  version: string;
  os: string;
}

/**
 * Reads the browser from a user agent string. Servers use the result for browser-specific behaviour, such
 * as Firefox choosing its own SDP track IDs, and this parameter is the only way they learn it.
 */
export function parseUserAgent(ua: string): BrowserInfo | undefined {
  const rules: Array<[string, RegExp]> = [
    ['Edge', /Edg(?:e|A|iOS)?\/(\d+[.\d]*)/],
    ['Opera', /OPR\/(\d+[.\d]*)/],
    ['Firefox', /Firefox\/(\d+[.\d]*)/],
    ['Chrome', /Chrome\/(\d+[.\d]*)/],
    ['Safari', /Version\/(\d+[.\d]*).*Safari\//],
  ];
  for (const [name, re] of rules) {
    const m = ua.match(re);
    if (!m) continue;
    let os = '';
    if (/Android/.test(ua)) os = 'android';
    else if (/iPhone|iPad|iPod/.test(ua)) os = 'ios';
    else if (/Mac OS X/.test(ua)) os = 'macos';
    else if (/Windows/.test(ua)) os = 'windows';
    else if (/Linux/.test(ua)) os = 'linux';
    return { name, version: m[1], os };
  }
  return undefined;
}

export function browserInfo(): BrowserInfo | undefined {
  return typeof navigator === 'undefined' ? undefined : parseUserAgent(navigator.userAgent);
}

export const isFirefox = () => browserInfo()?.name === 'Firefox';
export const isSafari = () => browserInfo()?.name === 'Safari';
