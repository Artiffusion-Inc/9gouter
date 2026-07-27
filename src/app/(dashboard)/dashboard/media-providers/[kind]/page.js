import ClientPage from "./ClientPage";
import { MEDIA_PROVIDER_KINDS } from "@/shared/constants/providers";

export const dynamic = "force-static";
// Prerender every media-provider kind, not just "image". With output:export a
// kind that has no prerendered .html falls through to the SPA index.html
// fallback in internal/adapter/transport/http/static.go (step 5) — the client
// then renders an empty dashboard shell, which reads as "redirects to
// dashboard / doesn't open". webSearch/webFetch prerender too; their ClientPage
// client-redirects to /dashboard/media-providers/web.
export function generateStaticParams() {
  return MEDIA_PROVIDER_KINDS.map((k) => ({ kind: k.id }));
}

export default function Page() { return <ClientPage />; }
