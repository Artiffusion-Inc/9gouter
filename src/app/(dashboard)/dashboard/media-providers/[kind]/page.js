import ClientPage from "./ClientPage";

export const dynamic = "force-static";
export function generateStaticParams() { return [{ kind: "image" }]; }

export default function Page() { return <ClientPage />; }
