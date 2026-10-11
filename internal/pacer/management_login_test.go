package pacer

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestManagementProxyLogin(t *testing.T) {
	node := os.Getenv("PACER_NODE")
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			t.Skip("Node required for management login verification")
		}
	}
	html, err := os.ReadFile("../management/index.html")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", managementLoginHarness)
	cmd.Stdin = strings.NewReader(string(html))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("management login: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}

const managementLoginHarness = `
const fs=require('node:fs'), vm=require('node:vm'), assert=require('node:assert/strict');
const html=fs.readFileSync(0,'utf8'), script=html.split('<script>')[1].split('</script>')[0];
assert(!html.includes('id="key"'));assert(!html.includes('id="signin"'));
const settle=async()=>{for(let i=0;i<20;i++)await new Promise(r=>setImmediate(r))};
async function scenario(status, action){
  const nodes=new Map(), calls=[], messages=[];
  const element=()=>({value:'',hidden:false,children:[],options:[],dataset:{},style:{},
    setAttribute(){},replaceChildren(){},append(){},close(){},reset(){},
    querySelector(){return {}},querySelectorAll(){return []}});
  for(const m of html.matchAll(/<[^>]*\bid="([^"]+)"[^>]*>/g))nodes.set(m[1],{...element(),hidden:/\bhidden\b/.test(m[0])});
  const el=id=>{assert(nodes.has(id),id);return nodes.get(id)};
  let release, onMessage;
  const sandbox={console,setTimeout,clearTimeout,location:{origin:'https://fixture.invalid'},
    document:{getElementById:el,querySelectorAll:()=>[],createElement:element},
    fetch:async(path,opts)=>{
      calls.push({path,opts});
      assert.equal(path,'/v0/management/plugins/pacer/state');
      assert.equal(opts.redirect,'error');assert.equal(opts.cache,'no-store');
      assert.equal(opts.credentials,'same-origin');
      assert.equal(opts.headers.Authorization,undefined);
      if(calls.length===1&&(action==='disconnect'||action==='bridge'))await new Promise(r=>release=r);
      const code=calls.length===1?status:200;
      return {ok:code===200,status:code,json:async()=>({routing:{mode:'dry-run',can_activate:false},accounts:[]})};
    }};
  sandbox.window={addEventListener(type,fn){if(type==='message')onMessage=fn}};
  sandbox.window.parent=action==='iframe'||action==='bridge'?{postMessage:m=>{
    messages.push(m);
    if(m.type==='pacer:request')setImmediate(()=>onMessage({source:sandbox.window.parent,origin:sandbox.location.origin,data:{type:'pacer:response',version:1,id:m.id,ok:true,data:{routing:{mode:'dry-run',can_activate:false},accounts:[]}}}));
  }}:sandbox.window;
  for(const k of ['localStorage','sessionStorage'])Object.defineProperty(sandbox,k,{get(){throw Error('credential persistence forbidden')}});
  vm.runInNewContext(script,sandbox,{timeout:5000});
  if(action==='disconnect'){assert(release);el('disconnect').onclick();release()}
  if(action==='bridge'){
    assert(release);onMessage({source:sandbox.window.parent,origin:sandbox.location.origin,data:{type:'pacer:bridge-ready',version:1}});release();
  }
  await settle();
  if(action==='iframe'||action==='bridge')assert.equal(messages[0].type,'pacer:hello');
  if(action==='bridge'){assert.equal(el('workspace').hidden,false);assert.equal(el('message').hidden,true);assert(messages.some(m=>m.type==='pacer:request'));return}
  assert.equal(calls.length,1,'exactly one automatic probe, no polling or duplicate state fetch');
  const authorized=status===200&&action!=='disconnect';
  assert.equal(el('login').hidden,authorized);assert.equal(el('workspace').hidden,!authorized);
  if(authorized){assert.equal(el('mode').value,'dry-run');el('disconnect').onclick();await settle();assert.equal(calls.length,1);assert.equal(el('workspace').hidden,true)}
  if(status!==200){
    assert.equal(el('message').hidden,false);
    assert.equal(el('reconnect').disabled,false);
    el('reconnect').onclick();await settle();
    assert.equal(calls.length,2);
    assert.equal(el('login').hidden,true);assert.equal(el('workspace').hidden,false);
  }
}
(async()=>{await scenario(200);await scenario(401);await scenario(403);await scenario(500);await scenario(200,'disconnect');await scenario(200,'iframe');await scenario(401,'iframe');await scenario(401,'bridge');console.log('PASS: proxy login, refusals, session retry, disconnect race, embedded proxy login and retry, legacy bridge race')})().catch(e=>{console.error(e);process.exitCode=1});
`
