import { TrackKind } from '../enums.js';
import { TrackEvent } from '../events.js';
import { RemoteTrack } from './RemoteTrack.js';

const HIDE_DELAY_MS = 100;
const RESIZE_DEBOUNCE_MS = 100;
const BACKGROUND_REACTION_MS = 5000;

export interface AdaptiveStreamSettings {
  /** Multiplier from layout size to video size. 'screen' uses the display's own density. */
  pixelDensity?: number | 'screen';
  /** Stop receiving while the page is in the background. */
  pauseVideoInBackground?: boolean;
}

interface ObservedElement {
  element: HTMLMediaElement;
  visible: boolean;
  pictureInPicture: boolean;
  /** When this element last became visible, to delay reacting to it going away again. */
  changedAt: number;
}

export class RemoteVideoTrack extends RemoteTrack {
  private adaptive?: AdaptiveStreamSettings;
  private observed: ObservedElement[] = [];
  private intersection?: IntersectionObserver;
  private resize?: ResizeObserver;
  private visible?: boolean;
  private lastDimensions?: { width: number; height: number };
  private hideTimer?: ReturnType<typeof setTimeout>;
  private resizeTimer?: ReturnType<typeof setTimeout>;
  private backgroundTimer?: ReturnType<typeof setTimeout>;
  private backgrounded = false;
  private onVisibilityChange?: () => void;

  constructor(mediaTrack: MediaStreamTrack, sid: string, receiver: RTCRtpReceiver, stream: MediaStream, adaptive?: AdaptiveStreamSettings) {
    super(mediaTrack, sid, TrackKind.Video, receiver, stream);
    this.adaptive = adaptive;
  }

  get isAdaptiveStream() {
    return this.adaptive !== undefined;
  }

  /**
   * With adaptive stream on, a track is only received while an attached element shows it, and only at that
   * element's size. A track attached to nothing receives nothing.
   */
  override attach(): HTMLMediaElement;
  override attach(element: HTMLMediaElement): HTMLMediaElement;
  override attach(element?: HTMLMediaElement): HTMLMediaElement {
    const el = element ? super.attach(element) : super.attach();
    if (this.adaptive && !this.observed.some((o) => o.element === el)) this.observe(el);
    return el;
  }

  override detach(): HTMLMediaElement[];
  override detach(element: HTMLMediaElement): HTMLMediaElement;
  override detach(element?: HTMLMediaElement): HTMLMediaElement | HTMLMediaElement[] {
    const detached = element ? super.detach(element) : super.detach();
    for (const el of Array.isArray(detached) ? detached : [detached]) this.unobserve(el);
    return detached;
  }

  private observe(el: HTMLMediaElement) {
    const entry: ObservedElement = { element: el, visible: this.isInViewport(el), pictureInPicture: false, changedAt: Date.now() };
    this.observed.push(entry);
    this.intersection ??= new IntersectionObserver(this.onIntersection, { root: null, rootMargin: '0px' });
    this.resize ??= new ResizeObserver(() => this.scheduleDimensions());
    this.intersection.observe(el);
    this.resize.observe(el);
    el.addEventListener('enterpictureinpicture', this.onEnterPip);
    el.addEventListener('leavepictureinpicture', this.onLeavePip);
    this.watchBackground();
    // Visibility goes out at once and dimensions shortly after. The order is safe: a size only narrows
    // what is already being received, and a new track should start showing something straight away.
    this.updateVisibility();
    this.scheduleDimensions();
  }

  private unobserve(el: HTMLMediaElement) {
    this.intersection?.unobserve(el);
    this.resize?.unobserve(el);
    el.removeEventListener('enterpictureinpicture', this.onEnterPip);
    el.removeEventListener('leavepictureinpicture', this.onLeavePip);
    const before = this.observed.length;
    this.observed = this.observed.filter((o) => o.element !== el);
    if (before !== this.observed.length) {
      this.updateVisibility();
      this.scheduleDimensions();
    }
  }

  private isInViewport(el: HTMLElement): boolean {
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && r.bottom > 0 && r.right > 0 && r.top < window.innerHeight && r.left < window.innerWidth;
  }

  private onIntersection = (entries: IntersectionObserverEntry[]) => {
    for (const ev of entries) {
      const o = this.observed.find((x) => x.element === ev.target);
      if (!o) continue;
      if (ev.isIntersecting && !o.visible) o.changedAt = Date.now();
      o.visible = ev.isIntersecting;
    }
    this.updateVisibility();
  };

  private onEnterPip = (ev: Event) => {
    const o = this.observed.find((x) => x.element === ev.target);
    if (o) o.pictureInPicture = true;
    this.updateVisibility();
  };

  private onLeavePip = (ev: Event) => {
    const o = this.observed.find((x) => x.element === ev.target);
    if (o) o.pictureInPicture = false;
    this.updateVisibility();
  };

  private watchBackground() {
    if (this.onVisibilityChange || this.adaptive?.pauseVideoInBackground === false) return;
    this.onVisibilityChange = () => {
      clearTimeout(this.backgroundTimer);
      if (document.visibilityState === 'hidden') {
        // Glancing at another tab should not cost a stall on coming back.
        this.backgroundTimer = setTimeout(() => {
          this.backgrounded = true;
          this.updateVisibility();
        }, BACKGROUND_REACTION_MS);
      } else {
        this.backgrounded = false;
        this.updateVisibility();
      }
    };
    document.addEventListener('visibilitychange', this.onVisibilityChange);
  }

  private updateVisibility() {
    clearTimeout(this.hideTimer);
    this.hideTimer = undefined;
    const shown = this.observed.some((o) => o.pictureInPicture || (o.visible && !this.backgrounded));
    if (shown) {
      this.setVisible(true);
      return;
    }
    // Hiding waits briefly, so an element moved in the page does not stop and restart the video.
    this.hideTimer = setTimeout(() => this.setVisible(false), HIDE_DELAY_MS);
  }

  private setVisible(visible: boolean) {
    if (this.visible === visible) return;
    this.visible = visible;
    this.emit(TrackEvent.VisibilityChanged, visible, this);
  }

  private scheduleDimensions() {
    clearTimeout(this.resizeTimer);
    this.resizeTimer = setTimeout(() => this.updateDimensions(), RESIZE_DEBOUNCE_MS);
  }

  private pixelDensity(): number {
    const d = this.adaptive?.pixelDensity;
    if (typeof d === 'number') return d;
    const screen = typeof window !== 'undefined' ? window.devicePixelRatio : 1;
    if (d === 'screen') return screen;
    // Beyond that, more pixels are not visible to anyone, and they use a lot of bandwidth.
    return screen > 2 ? 2 : 1;
  }

  private updateDimensions() {
    let width = 0;
    let height = 0;
    for (const { element } of this.observed) {
      const w = element.clientWidth;
      const h = element.clientHeight;
      if (w + h > width + height) {
        width = w;
        height = h;
      }
    }
    const density = this.pixelDensity();
    const next = { width: width * density, height: height * density };
    if (this.lastDimensions?.width === next.width && this.lastDimensions.height === next.height) return;
    this.lastDimensions = next;
    this.emit(TrackEvent.VideoDimensionsChanged, next, this);
  }

  override stop() {
    super.stop();
    clearTimeout(this.hideTimer);
    clearTimeout(this.resizeTimer);
    clearTimeout(this.backgroundTimer);
    this.intersection?.disconnect();
    this.resize?.disconnect();
    if (this.onVisibilityChange) document.removeEventListener('visibilitychange', this.onVisibilityChange);
    this.onVisibilityChange = undefined;
  }
}
