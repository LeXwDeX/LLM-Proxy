"use strict";
const assert = require("node:assert/strict");
const test = require("node:test");
const {createHarness} = require("../../../scripts/testing/admin_ui_test.cjs");

async function fixture() {
 const h=createHarness(),app=await h.run();
 app.modules.quota.loadQuotaEditorData=async()=>{};
 app.state.targets=[{name:"A",model_mappings:[{upstream:"m"}]}];
 return {h,app,pricing:app.modules.pricing};
}
function source(){return {id:"p",target:"A",model:"m",version:4,mode:"flat",base_rates:{input_per_1m_tokens:1,output_per_1m_tokens:2,cache_write_per_1m_tokens:0,cache_read_per_1m_tokens:1},conditions:[{id:"long",min_input_tokens:100,service_tier:"fast",rates:{input_per_1m_tokens:2,output_per_1m_tokens:4,cache_write_per_1m_tokens:0,cache_read_per_1m_tokens:1,image_input_per_1m_tokens:0,explicit_cache_read_per_1m_tokens:null}}]};}
test("editing retains version and complete conditions including missing versus zero",async()=>{
 const {h,pricing}=await fixture(),s=source();await pricing.openPricingModal(s,null,false);
 for(const id of ["ptInput","ptOutput","ptCacheWrite","ptCacheRead"])h.ids.get(id).value=String(h.ids.get(id).value);
 await h.ids.get("saveModelCostBtn").listeners.click[0]();
 const write=h.requests.find(r=>r.options?.method==="PUT"&&r.url.endsWith("/p"));assert.ok(write);
 const body=JSON.parse(write.options.body);
 assert.equal(body.version,4);assert.deepEqual(body.conditions,s.conditions);
});
test("copy and periods retain date exceptions and nested conditions",async()=>{
 const {app,pricing}=await fixture(),s=source();s.mode="schedule";s.start="2026-09-01T00:00:00Z";
 s.periods=[{weekdays:[1],start:"09:00",end:"12:00",rates:s.base_rates,conditions:s.conditions}];
 s.date_exceptions=[{start_date:"2026-10-01",end_date:"2026-10-08",rates:s.base_rates,conditions:s.conditions}];
 await pricing.openPricingModal(s,null,true);
 assert.deepEqual(JSON.parse(JSON.stringify(app.state.ptPeriods[0].conditions)),s.conditions);
 assert.deepEqual(JSON.parse(JSON.stringify(app.state.ptDateExceptions)),s.date_exceptions);
});

test("normal controls create and edit independent rates with schema supplied options",async()=>{
 const {h,app,pricing}=await fixture();await pricing.openPricingModal(null,null,false);
 assert.match(h.ids.get("ptContextRules").innerHTML,/<details[^>]* open><summary>条件价格 · 0 条/);
 pricing.pricingContextClick({target:{closest:()=>({dataset:{ptAddCondition:"base"}})}});
 function input(field,value){pricing.syncPricingContextInput({value,dataset:{ptContextScope:"base",ptConditionIndex:"0",ptConditionField:field}});}
 input("id","long");input("min_input_tokens","100");input("max_input_tokens","200");input("service_tier","future-tier");
 pricing.syncPricingContextInput({value:"0",dataset:{ptContextScope:"base",ptConditionIndex:"0",ptContextRate:"image_input_per_1m_tokens"}});
 pricing.syncPricingContextInput({value:"",dataset:{ptContextScope:"base",ptConditionIndex:"0",ptContextRate:"explicit_cache_read_per_1m_tokens"}});
 assert.equal(app.state.ptConditions[0].rates.image_input_per_1m_tokens,0);
 assert.equal(app.state.ptConditions[0].rates.explicit_cache_read_per_1m_tokens,null);
 pricing.renderPricingContext();
 assert.match(h.ids.get("ptContextRules").innerHTML,/未来档位/);
 assert.match(h.ids.get("ptContextRules").innerHTML,/总输入上限（不包含）/);
 assert.equal(pricing.validatePricingContext("flat").conditions[0].max_input_tokens,200);
 input("max_input_tokens","");assert.equal("max_input_tokens" in app.state.ptConditions[0],false);
});

test("validation rejects invalid versions, overlapping rules and calendar ranges",async()=>{
 const {h,app,pricing}=await fixture();await pricing.openPricingModal(source(),null,false);
 h.ids.get("ptVersion").value="1.5";assert.throws(()=>pricing.validatePricingContext("flat"),/规则版本/);
 h.ids.get("ptVersion").value="1";
 app.state.ptConditions.push({...app.state.ptConditions[0],id:"duplicate-scope"});
 assert.throws(()=>pricing.validatePricingContext("flat"),/重叠/);
 app.state.ptConditions=[];
 app.state.ptDateExceptions=[{start_date:"2030-02-30",end_date:"2030-03-02",rates:source().base_rates}];
 assert.throws(()=>pricing.validatePricingContext("schedule"),/有效范围/);
 app.state.ptDateExceptions=[{start_date:"2030-10-01",end_date:"2030-10-08",rates:source().base_rates}];
 assert.throws(()=>pricing.validatePricingContext("flat"),/分时段/);
 assert.equal(pricing.validatePricingContext("schedule").date_exceptions[0].end_date,"2030-10-08");
 app.state.ptDateExceptions.push({start_date:"2030-10-07",end_date:"2030-10-09",rates:source().base_rates});
 assert.throws(()=>pricing.validatePricingContext("schedule"),/不能重叠/);
});

test("missing price enum metadata blocks editor startup",async()=>{
 const h=createHarness({schemaError:false});const app=await h.run();
 const partial=JSON.parse(JSON.stringify(app.state.schema));delete partial.pricing_service_tiers;
 assert.throws(()=>app.modules.schema.validateSchema(partial),/pricing_service_tiers/);
});
