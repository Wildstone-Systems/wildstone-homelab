import assert from "node:assert/strict";
import {createElement as h} from "react";
import {renderToString} from "react-dom/server";
import {loadReviewPlans, PendingPlan} from "../src/views/Plans";
import {ApiError, type PlanRecord} from "../src/api";
async function main() {
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
 console.log("Owner review loading and rendering checks passed.");
}
main().catch(error=>{console.error(error);process.exitCode=1});
