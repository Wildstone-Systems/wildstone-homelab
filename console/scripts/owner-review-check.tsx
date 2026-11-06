import assert from "node:assert/strict";
import {createElement as h} from "react";
import {renderToString} from "react-dom/server";
import {loadReviewPlans, PendingPlan, DeploymentObservation} from "../src/views/Plans";
import {api, ApiError, type PlanRecord} from "../src/api";
async function main() {
 // Mixed-version upgrades may briefly read an older server's null collection.
 const originalFetch=globalThis.fetch;
 try {
  globalThis.fetch=async()=>new Response("null",{status:200,headers:{"Content-Type":"application/json"}});
  assert.deepEqual(await api.observed(),[],"empty observation response must be iterable");
  globalThis.fetch=async()=>new Response("[]",{status:200,headers:{"Content-Type":"application/json"}});
  assert.deepEqual(await api.observed(),[]);
 } finally {globalThis.fetch=originalFetch}
 const summary={id:"plan-42",state:"proposed",summary:"Morning routine",risk:"low",principal:"butler",createdAt:"2026-01-01T00:00:00Z"} as PlanRecord;
 const full={...summary,routine:{name:"morning",assignedButler:"review-agent-canary",revision:3,document:"review-document-canary",outputAudience:"aud_household",delivery:"review-delivery-canary"}};
 let queueRead=false;
 const plans=await loadReviewPlans({plans:async()=>[summary],ownerReviewQueue:async()=>{queueRead=true;return [full]},plan:async()=>{throw new Error("queue record was ignored")}});
 assert.ok(queueRead);
 const html=renderToString(h(PendingPlan,{plan:plans[0],onResolved:()=>{}}));
 for(const text of ["review-agent-canary","review-document-canary","aud_household","review-delivery-canary","version 3"])assert.ok(html.replace(/<!--.*?-->/g, "").includes(text),`approval omits ${text}`);
 let detailRead=false;
 await loadReviewPlans({plans:async()=>[summary],ownerReviewQueue:async()=>[],plan:async()=>{detailRead=true;return full}});
 assert.ok(detailRead,"unqueued infrastructure approval also needs full details");
 await assert.rejects(loadReviewPlans({plans:async()=>[summary],ownerReviewQueue:async()=>{throw new Error("offline")},plan:async()=>full}),/offline/);
 detailRead=false;
 await loadReviewPlans({plans:async()=>[summary],ownerReviewQueue:async()=>{throw new ApiError(403,"Owner required")},plan:async()=>{detailRead=true;return full}});
 assert.ok(detailRead,"bootstrap infrastructure Plans require full details despite unavailable Owner queue");
 for(const status of [401,500]) await assert.rejects(loadReviewPlans({plans:async()=>[summary],ownerReviewQueue:async()=>{throw new ApiError(status,"unavailable")},plan:async()=>full}));
 const config={name:"Review search",kind:"ddgs",endpointRef:"service/review-search",credentialRef:"secret/review-token",backends:["duckduckgo"],fallbackOrder:[],limits:{searchesPerTask:7,fetchesPerTask:11,requestTimeoutSeconds:23,downloadBytes:4096,extractedCharacters:8192,cacheSeconds:313}};
 for(const operation of ["create","update","delete"] as const) {
   const record:PlanRecord={...summary,research:{operation,providerId:"sr.review-provider",expectedConfigRevision:operation==="create"?"":"sr.review-revision",...(operation==="delete"?{}:{config})}};
   const rendered=renderToString(h(PendingPlan,{plan:record,onResolved:()=>{}})).replace(/<!--.*?-->/g, "");
   for(const text of [operation,"sr.review-provider","No service is provisioned","no Agent tool permissions are granted",...(operation==="delete"?["become unavailable","does not fall back"]:["Review search","service/review-search","secret/review-token","duckduckgo","Fallback order: None","7","11","23","4096","8192","313","must pass a new test"])])assert.ok(rendered.includes(text),`research approval omits ${text}`);
   assert.ok(!rendered.includes("hands the plan to the reconciler"));
   assert.ok(!rendered.includes("no expanded steps"));
   assert.ok(!/btn-primary[^>]*disabled/.test(rendered),"complete research change must be reviewable");
 }
 const incomplete:PlanRecord={...summary,research:{operation:"update",providerId:"sr.review-provider",expectedConfigRevision:"sr.review-revision"}};
 const incompleteHTML=renderToString(h(PendingPlan,{plan:incomplete,onResolved:()=>{}}));
 assert.match(incompleteHTML,/btn-primary[^>]*disabled/,"missing configuration must not enable approval");
 const observation={phase:"promoted",detail:"Local ref observed",productRevision:"example-product",promotedRevision:"example-promoted",delivery:"mirrored",reconciliation:"ready",application:"ready",observedAt:"2026-01-01T00:00:00Z",controllerObservedAt:"2026-01-01T00:00:00Z",controllerExpiresAt:"2026-01-01T00:00:30Z",sourceRevision:"example-source-revision",reconciledRevision:"example-applied-revision",helmGeneration:2,helmObservedGeneration:2};
 const live=renderToString(h(DeploymentObservation,{status:observation,now:Date.parse("2026-01-01T00:00:10Z")})).replace(/<!--.*?-->/g, "");
 for(const text of ["Reconciliation: ready","Application: ready","example-source-revision","example-applied-revision","2 / 2"])assert.ok(live.includes(text),`missing observed fact ${text}`);
 const staleObservation=renderToString(h(DeploymentObservation,{status:observation,now:Date.parse("2026-01-01T00:00:31Z")})).replace(/<!--.*?-->/g, "");
 assert.ok(staleObservation.includes("Reconciliation: stale"));assert.ok(staleObservation.includes("Application: unknown (stale observation)"));assert.ok(!staleObservation.includes("Application: ready"));
 const ciphertextRevision="c".repeat(64),recipientFingerprint=`sha256:${"d".repeat(64)}`;
 const deployment:PlanRecord={...summary,deploymentPlan:true,deployment:{instance:"example-app",operationId:"example-operation",previousProductRevision:"example-base",productRevision:"example-product",expectedDeploymentRevision:"example-parent",projectionDigest:"example-output-digest",profileDigest:"example-profile-digest",inventoryDigest:"example-inventory-digest",encryptedSecretRevision:"example-encrypted-version",package:"audiobooks-audiobookshelf",packageVersion:"1.0.0",chartSource:"https://example.invalid/charts",chartCommit:"example-immutable-commit",chartPath:"charts/example",chartVersion:"1.0.0",image:"example-image-digest",namespace:"example-namespace",release:"example-release",timezone:"UTC",desiredResource:"example-resource",files:{"source.yaml":"example-exact-source","release.yaml":"example-exact-release","kustomization.yaml":"example-exact-kustomization"},secret:{validation:"encrypted-reference-validated",target:{namespace:"example-namespace",name:"example-secret",key:"jwt-secret",type:"Opaque"},secretGeneration:7,ciphertextRevision,recipientFingerprints:[recipientFingerprint],outputPath:"secret.sops.yaml",outerKustomization:{namespace:"example-flux",name:"example-app"},decryptionSecretName:"example-age-identity",restartPolicy:"recreate-on-ciphertext-revision",restartEffect:"singleton-recreate-bounded-outage",privateFileCount:1}}};
 const reviewed=renderToString(h(PendingPlan,{plan:deployment,onResolved:()=>{}})).replace(/<!--.*?-->/g, "");
 for(const text of ["example-app","example-immutable-commit","example-image-digest","example-output-digest","example-profile-digest","example-inventory-digest","example-encrypted-version","example-product","example-parent","example-exact-source","example-exact-release","example-exact-kustomization","example-secret","generation","7",ciphertextRevision,recipientFingerprint,"secret.sops.yaml","withheld file","example-age-identity","private deployment Git","cannot create or read the age identity","retained"])assert.ok(reviewed.includes(text),`deployment review omits ${text}`);
 assert.ok(!/btn-primary[^>]*disabled/.test(reviewed),"complete deployment review must be approvable");
 const missing=renderToString(h(PendingPlan,{plan:{...deployment,deployment:undefined},onResolved:()=>{}}));
 assert.match(missing,/btn-primary[^>]*disabled/,"summary must not authorize deployment");
 const missingFile=renderToString(h(PendingPlan,{plan:{...deployment,deployment:{...deployment.deployment!,files:{"release.yaml":"partial output"}}},onResolved:()=>{}}));
 assert.match(missingFile,/btn-primary[^>]*disabled/,"partial output must not authorize deployment");
 const missingSecret=renderToString(h(PendingPlan,{plan:{...deployment,deployment:{...deployment.deployment!,secret:{...deployment.deployment!.secret!,recipientFingerprints:[]}}},onResolved:()=>{}}));
 assert.match(missingSecret,/btn-primary[^>]*disabled/,"incomplete managed Secret review must not authorize deployment");
 console.log("Owner review loading and rendering checks passed.");
}
main().catch(error=>{console.error(error);process.exitCode=1});
