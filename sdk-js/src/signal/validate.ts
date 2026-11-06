import { ConnectionError, ConnectionErrorReason } from '../errors.js';

const VALIDATE_TIMEOUT_MS = 3000;

/**
 * Browsers hide why a WebSocket upgrade failed, so the reason has to be asked for over HTTP.
 *
 * This client only speaks /rtc, so a generic 404 here is final. It means the URL is not a signalling
 * server at all, and there is no other path worth trying.
 */
export function errorFromValidateResponse(status: number, body: string): ConnectionError {
  if (status === 404 && body.includes('requested room does not exist')) {
    return new ConnectionError(body.trim(), ConnectionErrorReason.NotAllowed, { status });
  }
  if (status === 404) {
    return new ConnectionError(
      'the server does not serve /rtc: check the URL points at a signalling server',
      ConnectionErrorReason.ServiceNotFound,
      { status },
    );
  }
  if (status === 401 || status === 403) {
    return new ConnectionError(body.trim() || `not allowed (HTTP ${status})`, ConnectionErrorReason.NotAllowed, { status });
  }
  if (status >= 200 && status < 300) {
    // The server would accept this client, so the upgrade itself failed. A proxy that does not pass
    // WebSocket connections is the usual cause.
    return new ConnectionError(
      'could not establish signal connection: the server accepted the token, but the WebSocket failed',
      ConnectionErrorReason.InternalError,
      { status },
    );
  }
  return new ConnectionError(
    `could not establish signal connection: ${body.trim() || `HTTP ${status}`}`,
    ConnectionErrorReason.InternalError,
    { status },
  );
}

export async function explainFailedUpgrade(url: string, fetchFn: typeof fetch = fetch): Promise<ConnectionError> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), VALIDATE_TIMEOUT_MS);
  try {
    const res = await fetchFn(url, { signal: controller.signal });
    return errorFromValidateResponse(res.status, await res.text());
  } catch (e) {
    return new ConnectionError(
      `could not reach the server: ${e instanceof Error ? e.message : String(e)}`,
      ConnectionErrorReason.ServerUnreachable,
    );
  } finally {
    clearTimeout(timer);
  }
}
