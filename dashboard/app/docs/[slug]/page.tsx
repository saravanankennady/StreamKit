"use client";

import { notFound, useParams } from "next/navigation";
import { ApiReference } from "@/components/docs/api-reference";
import { DOC_PAGES } from "@/components/docs/pages";

export default function DocPage() {
  const { slug } = useParams<{ slug: string }>();
  if (slug === "api-reference") return <ApiReference />;
  const Page = DOC_PAGES[slug];
  if (!Page) notFound();
  return (
    <article className="prose-streamkit">
      <Page />
    </article>
  );
}
