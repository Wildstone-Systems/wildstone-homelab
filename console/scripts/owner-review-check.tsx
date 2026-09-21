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
 console.log("Owner review loading and rendering checks passed.");
}
main().catch(error=>{console.error(error);process.exitCode=1});
