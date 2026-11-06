import Link from "next/link";
import { DOC_NAV } from "@/components/docs/nav";
import { PRODUCT_NAME } from "@/lib/brand";

const BLURBS: Record<string, string> = {
  "getting-started": "Run the server, make a project, and get a browser into a room.",
  authentication: "Sessions, API keys and access tokens, and which one to use.",
  "rooms-and-participants": "Create rooms, set limits, mute and remove people from your backend.",
  publishing: "Send microphone and camera, and receive what others send.",
  "screen-sharing": "Share a screen, a window or a tab, with or without audio.",
  webhooks: "Events for your backend, with signatures you can verify.",
  "js-sdk": "The browser client, @elnova-streamkit/client.",
  "react-sdk": "Hooks and components, @elnova-streamkit/react.",
  recording: "Save rooms to files, and play them back.",
  egress: "Send rooms to files and RTMP destinations.",
  ingress: "Bring OBS and other streams into a room.",
  "api-reference": "Every REST route, read live from this server.",
  "self-hosting": "Ports, TURN, TLS and scaling out.",
  troubleshooting: "What an error means, and what to check first.",
};

export default function DocsHome() {
  return (
    <div className="max-w-4xl">
      <h1 className="text-2xl font-semibold tracking-tight">{PRODUCT_NAME} documentation</h1>
      <p className="mt-1 max-w-2xl text-ink-2">A self-hosted real-time video platform. The server is a single Go binary. Your browser clients connect with the SDK, and your backend controls rooms through a REST API.</p>
      <div className="mt-6 flex flex-col gap-6">
        {DOC_NAV.map(({ group, items }) => (
          <section key={group}>
            <h2 className="mb-2 text-xs font-normal text-ink-3">{group}</h2>
            <ul className="grid gap-px overflow-hidden rounded-xs border bg-line sm:grid-cols-2 sm:[&>li:last-child:nth-child(odd)]:col-span-2">
              {items.map((item) => (
                <li key={item.slug} className="bg-surface">
                  <Link href={`/docs/${item.slug}`} className="block h-full px-3 py-2.5 hover:bg-sunken">
                    <span className="block font-medium">{item.title}</span>
                    <span className="block text-sm text-ink-2">{BLURBS[item.slug]}</span>
                  </Link>
                </li>
              ))}
            </ul>
          </section>
        ))}
      </div>
    </div>
  );
}
