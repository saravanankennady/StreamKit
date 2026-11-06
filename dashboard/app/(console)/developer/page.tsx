"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { ApiKeysPanel } from "@/components/console/api-keys";
import { Page } from "@/components/console/page";
import { ProjectSettingsPanels } from "@/components/console/project-settings";
import { TokenGenerator } from "@/components/console/token-generator";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

export default function DeveloperPage() {
  const params = useSearchParams();
  const router = useRouter();
  const raw = params.get("tab");
  const tab = raw === "tokens" || raw === "settings" ? raw : "keys";
  return (
    <Page title="Developer settings" description="Credentials and defaults for this project.">
      <Tabs value={tab} onValueChange={(v) => router.replace(v === "keys" ? "/developer" : `/developer?tab=${v}`)}>
        <TabsList aria-label="Developer settings">
          <TabsTrigger value="keys">API keys</TabsTrigger>
          <TabsTrigger value="tokens">Token generator</TabsTrigger>
          <TabsTrigger value="settings">Project settings</TabsTrigger>
        </TabsList>
        <TabsContent value="keys" className="mt-4">
          <ApiKeysPanel />
        </TabsContent>
        <TabsContent value="tokens" className="mt-4">
          <TokenGenerator />
        </TabsContent>
        <TabsContent value="settings" className="mt-4">
          <ProjectSettingsPanels />
        </TabsContent>
      </Tabs>
    </Page>
  );
}
