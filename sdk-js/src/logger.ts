export enum LogLevel {
  trace = 0,
  debug = 1,
  info = 2,
  warn = 3,
  error = 4,
  silent = 5,
}

let level = LogLevel.warn;

/** Sets how much the SDK writes to the console. The default is warnings and errors. */
export function setLogLevel(next: LogLevel | keyof typeof LogLevel) {
  level = typeof next === 'string' ? LogLevel[next] : next;
}

function write(at: LogLevel, method: 'debug' | 'info' | 'warn' | 'error', args: unknown[]) {
  if (at >= level) console[method]('[Elnova StreamKit]', ...args);
}

export const log = {
  debug: (...args: unknown[]) => write(LogLevel.debug, 'debug', args),
  info: (...args: unknown[]) => write(LogLevel.info, 'info', args),
  warn: (...args: unknown[]) => write(LogLevel.warn, 'warn', args),
  error: (...args: unknown[]) => write(LogLevel.error, 'error', args),
};
