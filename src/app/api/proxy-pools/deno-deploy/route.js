import { NextResponse } from "next/server";
import { createProxyPool, updateProxyPool, findProxyPoolByNameAndType } from "@/models";

const DENO_V2_API = "https://api.deno.com/v2";

const DENO_RELAY_CODE = `Deno.serve(async (request) => {
  const target = request.headers.get("x-relay-target");
  const relayPath = request.headers.get("x-relay-path") || "/";

  if (!target) {
    return new Response(JSON.stringify({ error: "Missing x-relay-target header" }), {
      status: 400,
      headers: { "content-type": "application/json" },
    });
  }

  const targetUrl = target.replace(/\\/$/, "") + relayPath;
  const newHeaders = new Headers(request.headers);
  newHeaders.delete("x-relay-target");
  newHeaders.delete("x-relay-path");
  newHeaders.delete("host");

  const init = {
    method: request.method,
    headers: newHeaders,
  };

  if (request.method !== "GET" && request.method !== "HEAD") {
    init.body = request.body;
    init.duplex = "half";
  }

  try {
    const response = await fetch(targetUrl, init);
    // Deno fetch decompresses the upstream body but leaves content-encoding /
    // content-length / transfer-encoding on the response headers. Forwarding
    // them verbatim makes the downstream client gunzip already-plain bytes
    // → ZlibError "incorrect header check". Strip them.
    const respHeaders = new Headers(response.headers);
    respHeaders.delete("content-encoding");
    respHeaders.delete("content-length");
    respHeaders.delete("transfer-encoding");
    return new Response(response.body, {
      status: response.status,
      headers: respHeaders,
    });
  } catch (error) {
    return new Response(JSON.stringify({ error: error.message }), {
      status: 502,
      headers: { "content-type": "application/json" },
    });
  }
});`;

export async function POST(request) {
  try {
    const body = await request.json();
    const denoToken = body.denoToken?.trim();
    const orgDomain = body.orgDomain?.trim();
    const projectName = body.projectName?.trim() || `relay-${Date.now().toString(36)}`;

    if (!orgDomain) {
      return NextResponse.json({ error: "Organization domain is required" }, { status: 400 });
    }

    if (!denoToken) {
      return NextResponse.json({ error: "Deno Deploy API token is required" }, { status: 400 });
    }

    const headers = {
      Authorization: `Bearer ${denoToken}`,
      "Content-Type": "application/json",
    };

    const createAppRes = await fetch(`${DENO_V2_API}/apps`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        slug: projectName,
        labels: { "custom.kind": "9router-relay" },
        config: {
          install: "deno install",
          runtime: {
            type: "dynamic",
            entrypoint: "main.ts",
          },
        },
      }),
    });

    let app;
    let redeployed = false;
    if (createAppRes.ok) {
      app = await createAppRes.json();
    } else if (createAppRes.status === 409) {
      // App already exists → resolve its id by slug, then redeploy assets in place.
      const listRes = await fetch(`${DENO_V2_API}/apps`, { headers });
      if (!listRes.ok) {
        const text = await listRes.text().catch(() => "");
        return NextResponse.json(
          { error: `App exists but failed to list apps (${listRes.status}): ${text}` },
          { status: listRes.status }
        );
      }
      const listData = await listRes.json();
      const found = (listData.apps || []).find((a) => a.slug === projectName);
      if (!found) {
        return NextResponse.json(
          { error: `App "${projectName}" exists but slug not found in org listing` },
          { status: 409 }
        );
      }
      app = found;
      redeployed = true;
    } else {
      const text = await createAppRes.text().catch(() => "");
      return NextResponse.json(
        { error: `Failed to create app (${createAppRes.status}): ${text}` },
        { status: createAppRes.status }
      );
    }

    const deployRes = await fetch(`${DENO_V2_API}/apps/${app.id}/deploy`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        assets: {
          "main.ts": {
            kind: "file",
            content: DENO_RELAY_CODE,
            encoding: "utf-8",
          },
        },
      }),
    });

    if (!deployRes.ok) {
      const text = await deployRes.text().catch(() => "");
      console.error("Deno Deploy error:", deployRes.status, text);
      await fetch(`${DENO_V2_API}/apps/${app.id}`, {
        method: "DELETE",
        headers: { Authorization: `Bearer ${denoToken}` },
      }).catch(() => {});
      return NextResponse.json(
        { error: `Deploy failed (${deployRes.status}): ${text}` },
        { status: deployRes.status }
      );
    }

    const revision = await deployRes.json();
    const revisionId = revision.id;

    let status = revision.status;
    let attempts = 0;
    const maxAttempts = 30; // 30 * 2s = 60s max
    while (status === "queued" || status === "building") {
      if (attempts >= maxAttempts) {
        throw new Error("Deploy timed out after 60 seconds");
      }
      await new Promise((resolve) => setTimeout(resolve, 2000));
      const statusRes = await fetch(`${DENO_V2_API}/revisions/${revisionId}`, {
        headers: { Authorization: `Bearer ${denoToken}` },
      });
      if (!statusRes.ok) break;
      const statusData = await statusRes.json();
      status = statusData.status;
      attempts++;
    }

    if (status !== "succeeded") {
      await fetch(`${DENO_V2_API}/apps/${app.id}`, {
        method: "DELETE",
        headers: { Authorization: `Bearer ${denoToken}` },
      }).catch(() => {});
      return NextResponse.json(
        { error: `Deploy failed with status: ${status}` },
        { status: 500 }
      );
    }

    const orgSlug = orgDomain.split(".")[0];
    const deployUrl = `https://${projectName}.${orgSlug}.deno.net`;
    console.log("Deno deployUrl:", deployUrl);

    const existing = await findProxyPoolByNameAndType(projectName, "deno");
    let proxyPool;
    if (existing) {
      proxyPool = await updateProxyPool(existing.id, {
        proxyUrl: deployUrl,
        testStatus: "unknown",
        lastError: null,
        lastTestedAt: null,
        isActive: true,
      });
    } else {
      proxyPool = await createProxyPool({
        name: projectName,
        proxyUrl: deployUrl,
        type: "deno",
        noProxy: "",
        isActive: true,
        strictProxy: false,
      });
    }

    return NextResponse.json({ proxyPool, deployUrl, redeployed: redeployed || !!existing }, { status: existing ? 200 : 201 });
  } catch (error) {
    console.log("Error deploying Deno Deploy relay:", error);
    return NextResponse.json({ error: error.message || "Deploy failed" }, { status: 500 });
  }
}