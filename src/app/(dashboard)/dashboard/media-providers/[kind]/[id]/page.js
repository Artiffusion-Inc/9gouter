import ClientPage from "./ClientPage";
import { MEDIA_PROVIDER_KINDS } from "@/shared/constants/providers";

export const dynamic = "force-static";
// Prerender the detail page for every kind. The id:"_" is a shadow placeholder
// — internal/adapter/transport/http/static.go step 4 walks the _.html shadow for
// any /dashboard/media-providers/<kind>/<id> request. Without a prerendered
// shadow for a kind, that kind's detail route falls through to the SPA
// index.html fallback and renders an empty dashboard shell.
export function generateStaticParams() {
  return MEDIA_PROVIDER_KINDS.map((k) => ({ kind: k.id, id: "_" }));
}

export default function Page() { return <ClientPage />; }
