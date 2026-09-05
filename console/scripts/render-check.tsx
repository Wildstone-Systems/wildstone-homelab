// A render check for the Console, without a browser.
//
// `tsc --noEmit` proves the types line up and says nothing about whether
// the page renders. This renders it with react-dom/server against the
// exact JSON GET /api/v1/packages serves, which is the shape that
// actually broke once: pkgspec.ParamDecl had yaml tags only, so the API
// served {"Name","Required","Default"} while the client read
// name/required/default, and the generated form rendered every field
// blank on a 200 response.
//
// useEffect does not run under renderToString, so the data-loading half
// is not covered here. The generated form is, and it is where the logic
// lives. Run with: npm run render-check
import { renderToString } from "react-dom/server";
import { createElement as h } from "react";
import { Services, InstallForm } from "../src/views/Services";

// Exactly what GET /api/v1/packages serves, copied from the live VM.
const pkg = {
  name: "documents-paperless", version: "1.0.0", capability: "documents",
  enabled: false,
  params: [
    { name: "public_domain", required: true },
    { name: "docs_subdomain", default: "paperless" },
    { name: "node", required: true },
    { name: "identity_node" },
    { name: "timezone", default: "UTC" },
  ],
};

let failures = 0;
function check(label: string, cond: boolean, detail = "") {
  if (!cond) { failures++; console.log("FAIL:", label, detail); }
  else console.log("ok:", label);
}

const form = renderToString(h(InstallForm, { pkg: pkg as never, onDone: () => {} }));
check("form renders without throwing", form.length > 0);
for (const p of pkg.params) check(`input for ${p.name}`, form.includes(p.name));
// React SSR separates adjacent text nodes with <!-- -->, so the marker
// is not literally adjacent in the HTML. Assert on the rendered label as
// a whole rather than on a string only a browser would concatenate.
const label = /<label[^>]*>(.*?)<\/label>/.exec(form)?.[1] ?? "";
check("required params marked", label.replace(/<!--.*?-->/g, "") === "public_domain *", JSON.stringify(label));
check("defaults shown as placeholders", form.includes('placeholder="paperless"'));
check("preview gated until required filled", form.includes("fill in public_domain, node"));

const list = renderToString(h(Services));
check("Services renders without throwing", list.length > 0);

console.log(failures === 0 ? "\nALL CHECKS PASSED" : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
