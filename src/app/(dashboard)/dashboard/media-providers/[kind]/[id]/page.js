import ClientPage from "./ClientPage";

export const dynamic = "force-static";
export function generateStaticParams() { return [{ kind: "image", id: "_" }]; }

export default function Page() { return <ClientPage />; }
