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
const settle=async()=>{for(let i=0;i<20;i++)await new Promise(r=>setImmediate(r))};
async function scenario(status, action){
  const nodes=new Map(), calls=[], messages=[];
  const element=()=>({value:'',hidden:false,children:[],options:[],dataset:{},style:{},
    setAttribute(){},replaceChildren(){},append(){},close(){},reset(){},
    querySelector(){return {}},querySelectorAll(){return []}});
  for(const m of html.matchAll(/<[^>]*\bid="([^"]+)"[^>]*>/g))nodes.set(m[1],{...element(),hidden:/\bhidden\b/.test(m[0])});
  const el=id=>{assert(nodes.has(id),id);return nodes.get(id)};
  let release;
  const sandbox={console,setTimeout,clearTimeout,location:{origin:'https://fixture.invalid'},
    document:{getElementById:el,querySelectorAll:()=>[],createElement:element},
    fetch:async(path,opts)=>{
      calls.push({path,opts});
      assert.equal(path,'/v0/management/plugins/pacer/state');
      assert.equal(opts.redirect,'error');assert.equal(opts.cache,'no-store');
      assert.equal(opts.credentials,'same-origin');
      if(calls.length===1){assert.equal(opts.headers.Authorization,undefined);if(action==='disconnect')await new Promise(r=>release=r)}
      const code=calls.length===1?status:200;
      return {ok:code===200,status:code,json:async()=>({routing:{mode:'dry-run',can_activate:false},accounts:[]})};
    }};
  sandbox.window={addEventListener(){}};
  sandbox.window.parent=action==='iframe'?{postMessage:m=>messages.push(m)}:sandbox.window;
  for(const k of ['localStorage','sessionStorage'])Object.defineProperty(sandbox,k,{get(){throw Error('credential persistence forbidden')}});
  vm.runInNewContext(script,sandbox,{timeout:5000});
  if(action==='disconnect'){assert(release);el('disconnect').onclick();release()}
  await settle();
  if(action==='iframe'){
    assert.equal(calls.length,0);assert.equal(messages[0].type,'pacer:hello');return;
  }
  assert.equal(calls.length,1,'exactly one automatic probe, no polling or duplicate state fetch');
  const authorized=status===200&&action!=='disconnect';
  assert.equal(el('login').hidden,authorized);assert.equal(el('workspace').hidden,!authorized);
  if(authorized){assert.equal(el('mode').value,'dry-run');el('disconnect').onclick();await settle();assert.equal(calls.length,1);assert.equal(el('workspace').hidden,true)}
  if(status===401){
    el('key').value='synthetic-admin';await el('signin').onsubmit({preventDefault(){}});
    assert.equal(calls.length,2);assert.equal(calls[1].opts.headers.Authorization,'Bearer synthetic-admin');
    assert.equal(el('login').hidden,true);assert.equal(el('workspace').hidden,false);assert.equal(el('key').value,'');
  }
}
(async()=>{await scenario(200);await scenario(401);await scenario(403);await scenario(500);await scenario(200,'disconnect');await scenario(200,'iframe');console.log('PASS: proxy login, refusals, manual login, disconnect race, iframe bridge')})().catch(e=>{console.error(e);process.exitCode=1});
`
