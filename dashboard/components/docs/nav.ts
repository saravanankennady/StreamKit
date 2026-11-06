// In its own file, because both server and client components read it.
export const DOC_NAV: { group: string; items: { slug: string; title: string }[] }[] = [
  {
    group: "Start here",
    items: [
      { slug: "getting-started", title: "Getting started" },
      { slug: "authentication", title: "Authentication" },
    ],
  },
  {
    group: "Build",
    items: [
      { slug: "rooms-and-participants", title: "Rooms and participants" },
      { slug: "publishing", title: "Publishing media" },
      { slug: "screen-sharing", title: "Screen sharing" },
      { slug: "webhooks", title: "Webhooks" },
    ],
  },
  {
    group: "SDKs",
    items: [
      { slug: "js-sdk", title: "JavaScript SDK" },
      { slug: "react-sdk", title: "React SDK" },
    ],
  },
  {
    group: "Media services",
    items: [
      { slug: "recording", title: "Recording" },
      { slug: "egress", title: "Egress" },
      { slug: "ingress", title: "Ingress" },
    ],
  },
  {
    group: "Reference",
    items: [
      { slug: "api-reference", title: "API reference" },
      { slug: "self-hosting", title: "Self-hosted deployment" },
      { slug: "troubleshooting", title: "Troubleshooting" },
    ],
  },
];
