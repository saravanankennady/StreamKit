import { Activity, BarChart3, Boxes, CircleDot, Code2, Clapperboard, FileText, Gauge, Send, Server, Upload, Users, Webhook, type LucideIcon } from "lucide-react";

export interface NavItem {
  href: string;
  label: string;
  icon: LucideIcon;
  /** A live count from the monitor to show beside the label. */
  count?: "rooms" | "participants";
}

export const NAV: { group: string; items: NavItem[] }[] = [
  {
    group: "Operate",
    items: [
      { href: "/overview", label: "Overview", icon: Gauge },
      { href: "/monitor", label: "Live monitor", icon: Activity },
      { href: "/rooms", label: "Rooms", icon: Boxes, count: "rooms" },
      { href: "/participants", label: "Participants", icon: Users, count: "participants" },
    ],
  },
  {
    group: "Media",
    items: [
      { href: "/recordings", label: "Recordings", icon: Clapperboard },
      { href: "/egress", label: "Egress", icon: Send },
      { href: "/ingress", label: "Ingress", icon: Upload },
    ],
  },
  {
    group: "Observe",
    items: [
      { href: "/analytics", label: "Analytics", icon: BarChart3 },
      { href: "/logs", label: "Logs and events", icon: FileText },
      { href: "/servers", label: "Servers", icon: Server },
    ],
  },
  {
    group: "Configure",
    items: [
      { href: "/projects", label: "Projects", icon: CircleDot },
      { href: "/developer", label: "Developer settings", icon: Code2 },
      { href: "/webhooks", label: "Webhooks", icon: Webhook },
    ],
  },
];
