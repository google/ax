// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	osConfig "os/exec"
	"runtime"
	"time"

	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/proto"
	"github.com/spf13/cobra"
)

var (
	conversationID  string
	traceServerAddr string
	traceConfigFile string
)

var traceCmd = &cobra.Command{
	Use:   "trace",
	Short: "View the execution trace",
	RunE:  runTrace,
}

func init() {
	traceCmd.Flags().StringVar(&conversationID, "conversation", "", "Conversation ID")
	traceCmd.Flags().StringVar(&traceServerAddr, "addr", "localhost:8080", "Server address to listen on")
	traceCmd.Flags().StringVar(&traceConfigFile, "config", "ax.yaml", "Path to YAML configuration file")
	traceCmd.MarkFlagRequired("conversation")
}

func runTrace(cmd *cobra.Command, args []string) error {
	cfg, err := newConfig(cmd, traceConfigFile)
	if err != nil {
		return err
	}

	// Load trace data
	data, err := loadTraceData(cmd.Context(), cfg, conversationID)
	if err != nil {
		return fmt.Errorf("error loading trace data: %w", err)
	}

	if len(data.Execs) == 0 {
		return fmt.Errorf("no trace data found")
	}

	// Start HTTP server on specified address
	listener, err := net.Listen("tcp", traceServerAddr)
	if err != nil {
		return fmt.Errorf("failed to bind server (try another using --server): %w", err)
	}

	return serveTraceUI(listener, data, indexHTML)
}

type Text struct {
	Text string `json:"text"`
}

type Approval struct {
	Approved bool `json:"approved"`
}

type Confirmation struct {
	ID       string    `json:"id"`
	Question string    `json:"question,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
}

type Content struct {
	Role         string        `json:"role"`
	Text         *Text         `json:"text,omitempty"`
	Confirmation *Confirmation `json:"confirmation,omitempty"`
}

type ExecutionEvent struct {
	ExecID    string    `json:"exec_id"`
	AgentID   string    `json:"agent_id"`
	Inputs    []Content `json:"inputs"`
	Outputs   []Content `json:"outputs"`
	State     string    `json:"state"`
	Timestamp time.Time `json:"timestamp"`
}

type ExecTrace struct {
	ExecID  string           `json:"exec_id"`
	AgentID string           `json:"agent_id"`
	Events  []ExecutionEvent `json:"events"`
}

type TraceData struct {
	ConversationID string      `json:"conversation_id"`
	RootExecID     string      `json:"root_exec_id"`
	Execs          []ExecTrace `json:"execs"`
}

func loadTraceData(ctx context.Context, cfg *cliutil.Config, convID string) (*TraceData, error) {
	events, rootExecID, execIDs, err := fetch(ctx, cfg, convID)
	if err != nil {
		return nil, err
	}

	// TODO(jbd): Trace view incorrectly displays graph executions. We are not
	// changing the EventLog interface to fix this because the executor is being
	// removed soon in favor of a linear execution model. We will adopt a different
	// style of visualization once that's done.
	return &TraceData{
		ConversationID: convID,
		RootExecID:     rootExecID,
		Execs:          buildExecTraces(execIDs, events),
	}, nil
}

func fetch(ctx context.Context, cfg *cliutil.Config, convID string) ([]*proto.ExecutionEvent, string, []string, error) {
	evLog, err := executor.OpenSQLiteEventLog(cfg.EventLog.SQLiteConfig.Filename)
	if err != nil {
		return nil, "", nil, fmt.Errorf("could not open sqlite eventlog: %w", err)
	}
	defer evLog.Close()

	convEvents, err := evLog.Events(ctx, convID)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to query conversation events: %w", err)
	}

	var execIDs []string
	seen := make(map[string]bool)
	for _, ev := range convEvents {
		if ev.ExecId != "" && !seen[ev.ExecId] {
			execIDs = append(execIDs, ev.ExecId)
			seen[ev.ExecId] = true
		}
	}

	if len(execIDs) == 0 {
		return nil, "", nil, fmt.Errorf("no executions found for conversation: %s", convID)
	}

	var allEvents []*proto.ExecutionEvent
	for _, eID := range execIDs {
		events, err := evLog.ExecEvents(ctx, eID)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to query events for exec %s: %w", eID, err)
		}
		allEvents = append(allEvents, events...)
	}

	// Use the first execID as the rootExecID as requested by user
	rootExecID := execIDs[0]

	return allEvents, rootExecID, execIDs, nil
}

func buildExecTraces(execIDs []string, events []*proto.ExecutionEvent) []ExecTrace {
	execsMap := make(map[string][]ExecutionEvent)

	for _, protoEv := range events {
		exID := protoEv.ExecId
		ev := extractExecutionEvent(exID, protoEv)
		execsMap[exID] = append(execsMap[exID], ev)
	}

	var execs []ExecTrace
	for _, execID := range execIDs {
		if evs, ok := execsMap[execID]; ok {
			agentID := ""
			for _, ev := range evs {
				if ev.AgentID != "" {
					agentID = ev.AgentID
					break
				}
			}
			execs = append(execs, ExecTrace{
				ExecID:  execID,
				AgentID: agentID,
				Events:  evs,
			})
		}
	}

	return execs
}

func extractMsgs(protoContents []*proto.Message) []Content {
	var results []Content
	for _, c := range protoContents {
		// Skip messages flagged as internal-only.
		if c.GetInternalOnly() {
			continue
		}
		content := Content{Role: c.Role}
		msgContent := c.GetContent()
		if msgContent == nil {
			continue
		}
		if textC := msgContent.GetText(); textC != nil {
			content.Text = &Text{Text: textC.Text}
		} else if conf := msgContent.GetConfirmation(); conf != nil {
			content.Confirmation = &Confirmation{
				ID:       conf.Id,
				Question: conf.Question,
			}
			if app := conf.GetApproval(); app != nil {
				content.Confirmation.Approval = &Approval{Approved: app.Approved}
			} else if dec := conf.GetDecline(); dec != nil {
				content.Confirmation.Approval = &Approval{Approved: !dec.Declined}
			}
		}
		results = append(results, content)
	}
	return results
}

func extractExecutionEvent(execID string, protoEv *proto.ExecutionEvent) ExecutionEvent {
	ev := ExecutionEvent{
		ExecID:  execID,
		AgentID: protoEv.AgentId,
	}
	if protoEv.Timestamp != nil {
		ev.Timestamp = protoEv.Timestamp.AsTime()
	}

	ev.State = fmt.Sprint(protoEv.State)
	ev.Outputs = extractMsgs(protoEv.Outputs)
	ev.Inputs = extractMsgs(protoEv.Inputs)

	return ev
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = osConfig.Command("xdg-open", url).Start()
	case "windows":
		err = osConfig.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = osConfig.Command("open", url).Start()
	}
	if err != nil {
		fmt.Printf("Failed to open browser: %v\n", err)
	}
}

func serveTraceUI(listener net.Listener, data *TraceData, rawHTML string) error {
	addr := listener.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err == nil && (host == "::" || host == "0.0.0.0" || host == "" || host == "[::]") {
		addr = fmt.Sprintf("localhost:%s", port)
	}
	url := fmt.Sprintf("http://%s", addr)

	fmt.Printf("Starting trace viewer for conversation %s...\n", data.ConversationID)
	fmt.Printf("Opening browser to %s\n", url)
	fmt.Printf("Press Ctrl+C to exit.\n")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/trace", handleTrace(data))
	mux.HandleFunc("/", handleIndex(rawHTML))

	go openBrowser(url)

	return http.Serve(listener, mux)
}

func handleTrace(data *TraceData) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}
}

func handleIndex(rawHTML string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, rawHTML)
	}
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Trace Viewer</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#f0f2f5;color:#1a1a2e;height:100vh;display:flex;flex-direction:column;overflow:hidden}

.hdr{background:#1e1e2e;color:#cdd6f4;padding:10px 20px;display:flex;align-items:center;gap:10px;flex-shrink:0}
.hdr-logo{font-size:18px}
.hdr h1{font-size:15px;font-weight:600;letter-spacing:.3px}

.layout{display:flex;flex:1;overflow:hidden}

.main{flex:1;min-height:0;overflow-y:auto;padding:20px;display:flex;flex-direction:column;gap:14px}
.empty{display:flex;align-items:center;justify-content:center;flex:1;color:#6c7086;font-size:14px}
.loading{display:flex;align-items:center;justify-content:center;height:120px;color:#6c7086}

.trace-hdr .label{font-size:11px;color:#6c7086;text-transform:uppercase;letter-spacing:.5px;margin-bottom:4px}
.trace-hdr .exec-id{font-family:monospace;font-size:15px;font-weight:700;color:#1a1a2e}

.timeline-card{background:#fff;border-radius:10px;padding:14px 18px;box-shadow:0 1px 4px rgba(0,0,0,.08);flex-shrink:0}
.card-title{font-size:10px;text-transform:uppercase;letter-spacing:.8px;color:#6c7086;margin-bottom:10px;font-weight:600}
.tl-rows{display:flex;flex-direction:column;gap:6px}
.tl-row{display:flex;align-items:center;gap:10px}
.tl-label{width:220px;flex-shrink:0;font-size:11px;font-family:monospace;color:#4a4a6a;word-break:break-all}
.tl-track{flex:1;height:18px;background:#f0f0f0;border-radius:4px;position:relative}
.tl-bar{height:100%;border-radius:4px;position:absolute;display:flex;align-items:center;padding:0 5px;min-width:6px;cursor:default}
.tl-bar span{font-size:10px;color:#fff;white-space:nowrap;overflow:hidden;text-shadow:0 0 3px rgba(0,0,0,.3)}
.tl-bar.c0{background:#89b4fa}
.tl-bar.c1{background:#a6e3a1}
.tl-bar.c2{background:#fab387}
.tl-bar.c3{background:#cba6f7}
.tl-bar.c4{background:#f38ba8}
.tl-bar.c5{background:#94e2d5}
.tl-bar.c6{background:#f9e2af}

.exec-card{background:#fff;border-radius:10px;box-shadow:0 1px 4px rgba(0,0,0,.08);overflow:hidden;flex-shrink:0}
.exec-card-hdr{padding:11px 16px;display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;background:#fafafa;border-bottom:1px solid #f0f0f0}
.exec-card-hdr:hover{background:#f4f4f8}
.expand-ico{color:#6c7086;font-size:11px;transition:transform .15s;flex-shrink:0}
.exec-card-hdr.collapsed .expand-ico{transform:rotate(-90deg)}
.exec-card-body.hidden{display:none}
.exec-name{font-family:monospace;font-size:13px;font-weight:700;color:#1e1e2e}
.agent-badge{font-size:12px;padding:2px 8px;border-radius:10px;background:#ede9fe;color:#6d28d9;font-family:monospace}
.state-badge{font-size:11px;padding:2px 8px;border-radius:10px;margin-left:auto;font-weight:600}
.state-badge.completed{background:#dcfce7;color:#166534}
.state-badge.pending{background:#fef3c7;color:#92400e}
.state-badge.failed{background:#fee2e2;color:#991b1b}
.exec-card-body{}

.event{border-top:1px solid #f5f5f5}
.state-evt{padding:7px 18px;font-size:11px;color:#7c7ca0;font-style:italic;background:#fafafe;display:flex;align-items:center;gap:8px}
.state-evt .etime{font-family:monospace;color:#a0a0c0}
.evt-meta{padding:8px 18px 2px;display:flex;align-items:center;gap:8px}
.etime{font-size:11px;color:#a0a0b0;font-family:monospace}
.evt-section-label{font-size:10px;text-transform:uppercase;letter-spacing:.5px;color:#a0a0b0}

.content-list{padding:4px 18px 10px;display:flex;flex-direction:column;gap:5px}
.content-item{border-radius:7px;overflow:hidden;border:1px solid transparent}

.role-user{border-color:#dbeafe}
.role-user .c-hdr{background:#dbeafe;color:#1d4ed8}
.role-user .c-body{background:#eff6ff}

.role-model{border-color:#dcfce7}
.role-model .c-hdr{background:#dcfce7;color:#166534}
.role-model .c-body{background:#f0fdf4}

.role-agent{border-color:#ccfbf1}
.role-agent .c-hdr{background:#ccfbf1;color:#0f766e}
.role-agent .c-body{background:#f0fdfa}

.role-assistant{border-color:#f3e8ff}
.role-assistant .c-hdr{background:#f3e8ff;color:#7e22ce}
.role-assistant .c-body{background:#faf5ff}

.role-unspecified{border-color:#e5e7eb}
.role-unspecified .c-hdr{background:#f3f4f6;color:#4b5563}
.role-unspecified .c-body{background:#f9fafb}

.role-unknown{border-color:#e5e7eb}
.role-unknown .c-hdr{background:#f3f4f6;color:#4b5563}
.role-unknown .c-body{background:#f9fafb}

.c-hdr{font-size:10px;font-weight:700;padding:3px 10px;text-transform:uppercase;letter-spacing:.5px}
.c-body{padding:8px 10px;font-size:13px;line-height:1.6}

.txt{white-space:pre-wrap;word-break:break-word}
.txt pre{background:#1e1e2e;color:#cdd6f4;padding:10px 14px;border-radius:6px;overflow-x:auto;font-size:12px;line-height:1.5;margin:4px 0;font-family:'Fira Code','Cascadia Code',monospace}
.txt code{font-family:'Fira Code','Cascadia Code',monospace}

.conf-q{background:#fffbeb;border:1px solid #fcd34d;border-radius:6px;padding:7px 12px;font-size:13px;color:#92400e}
.conf-q::before{content:"? ";font-weight:700;color:#d97706}
.conf-approved{font-size:12px;color:#166534;font-weight:600}
.conf-approved::before{content:"✓ Approved"}
.conf-denied{font-size:12px;color:#991b1b;font-weight:600}
.conf-denied::before{content:"✗ Denied"}
</style>
</head>
<body>
<div class="hdr">
  <span class="hdr-logo">⬡</span>
  <h1>Trace Viewer</h1>
</div>
<div class="layout">
  <div class="main" id="main">
    <div class="loading">Loading trace...</div>
  </div>
</div>

<script>
function esc(s){
  if(s==null)return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function fmtTime(ts){
  const d=new Date(ts);
  return d.toLocaleTimeString('en-US',{hour12:false,hour:'2-digit',minute:'2-digit',second:'2-digit',fractionalSecondDigits:3});
}

const BT=String.fromCharCode(96);
const FENCE=BT+BT+BT;
const reSplit=new RegExp('('+FENCE+'(?:[^\\n]*)?\\n[\\s\\S]*?'+FENCE+')');
const reMatch=new RegExp('^'+FENCE+'([^\\n]*)?\\n([\\s\\S]*)'+FENCE+'$');
function renderText(raw){
  const parts=raw.split(reSplit);
  return parts.map(part=>{
    const m=part.match(reMatch);
    if(m){return '<pre><code>'+esc(m[2])+'</code></pre>';}
    return '<span class="txt">'+esc(part)+'</span>';
  }).join('');
}

function renderContent(c){
  const role=c.role||'unknown';
  let body='';
  if(c.text){
    body='<div class="c-body"><div class="txt">'+renderText(c.text.text)+'</div></div>';
  } else if(c.confirmation){
    const cf=c.confirmation;
    if(cf.question){
      body='<div class="c-body"><div class="conf-q">'+esc(cf.question)+'</div></div>';
    } else if(cf.approval!=null){
      const cls=cf.approval.approved?'conf-approved':'conf-denied';
      body='<div class="c-body"><div class="'+cls+'"></div></div>';
    }
  }
  if(!body)return '';
  return '<div class="content-item role-'+esc(role)+'"><div class="c-hdr">'+esc(role)+'</div>'+body+'</div>';
}

function renderEvent(ev){
  const time=fmtTime(ev.timestamp);
  const hasIn=ev.inputs&&ev.inputs.length>0;
  const hasOut=ev.outputs&&ev.outputs.length>0;

  // State-only event
  if(!hasIn&&!hasOut&&ev.state){
    return '<div class="event"><div class="state-evt"><span class="etime">'+esc(time)+'</span>State → '+esc(ev.state)+'</div></div>';
  }

  let html='<div class="event"><div class="evt-meta"><span class="etime">'+esc(time)+'</span></div>';
  if(hasIn){
    html+='<div class="content-list">';
    ev.inputs.forEach(c=>{html+=renderContent(c);});
    html+='</div>';
  }
  if(hasOut){
    html+='<div class="content-list">';
    ev.outputs.forEach(c=>{html+=renderContent(c);});
    html+='</div>';
  }
  html+='</div>';
  return html;
}

function getLastState(events){
  for(let i=events.length-1;i>=0;i--){
    if(events[i].state)return events[i].state;
  }
  return null;
}

function renderTrace(data){
  // Build timeline
  const execTimes=data.execs.map(e=>{
    const ts=e.events.map(ev=>new Date(ev.timestamp).getTime());
    const start=ts.length?Math.min(...ts):null;
    const end=ts.length?Math.max(...ts):null;
    return{execID:e.exec_id,start,end};
  });
  execTimes.sort((a,b)=>{
    if(a.start==null)return 1;
    if(b.start==null)return -1;
    return a.start-b.start;
  });
  let minT=null,maxT=null;
  execTimes.forEach(({start,end})=>{
    if(start!=null){
      if(minT==null||start<minT)minT=start;
      if(maxT==null||end>maxT)maxT=end;
    }
  });
  const span=maxT-minT||1;

  let html='<div class="trace-hdr"><div class="label">Conversation</div><div class="exec-id">'+esc(data.conversation_id)+'</div></div>';

  html+='<div class="timeline-card"><div class="card-title">Timeline</div><div class="tl-rows">';
  execTimes.forEach(({execID,start,end},i)=>{
    const shortName=execID===data.root_exec_id?'root':execID.slice(data.root_exec_id.length+1);
    const left=start!=null?((start-minT)/span*100).toFixed(2):0;
    const width=start!=null?Math.max((end-start)/span*100,0.4).toFixed(2):0;
    const dur=end-start;
    const durStr=dur<1000?dur+'ms':(dur/1000).toFixed(2)+'s';
    const colorCls='c'+(i%7);
    html+='<div class="tl-row">'
      +'<div class="tl-label" title="'+esc(execID)+'">'+esc(shortName)+'</div>'
      +'<div class="tl-track"><div class="tl-bar '+colorCls+'" style="left:'+left+'%;width:'+width+'%;cursor:pointer" title="'+esc(durStr)+'" onclick="scrollToCard(\''+esc(execID)+'\')"><span>'+esc(durStr)+'</span></div></div>'
      +'</div>';
  });
  html+='</div></div>';

  // Exec sections
  data.execs.forEach((exec,i)=>{
    const shortName=exec.exec_id===data.root_exec_id?'root':exec.exec_id.slice(data.root_exec_id.length+1);
    const lastState=getLastState(exec.events);
    const stateClass=lastState?lastState.toLowerCase().replace('state_',''):'';
    const stateBadge=lastState?'<span class="state-badge '+esc(stateClass)+'">'+esc(lastState.replace('STATE_',''))+'</span>':'';

    html+='<div class="exec-card" id="card-'+esc(exec.exec_id)+'">'
      +'<div class="exec-card-hdr collapsed" onclick="toggleCard(this)">'
      +'<span class="expand-ico">▼</span>'
      +'<span class="exec-name">'+esc(shortName)+'</span>'
      +'<span class="agent-badge">'+esc(exec.agent_id||'—')+'</span>'
      +stateBadge
      +'</div>'
      +'<div class="exec-card-body hidden">';
    exec.events.forEach(ev=>{html+=renderEvent(ev);});
    html+='</div></div>';
  });

  document.getElementById('main').innerHTML=html;
}

function scrollToCard(execID){
  const el=document.getElementById('card-'+execID);
  if(!el)return;
  const hdr=el.querySelector('.exec-card-hdr');
  const body=el.querySelector('.exec-card-body');
  if(hdr&&hdr.classList.contains('collapsed')){
    hdr.classList.remove('collapsed');
    body.classList.remove('hidden');
  }
  el.scrollIntoView({behavior:'smooth',block:'start'});
}

function toggleCard(hdr){
  hdr.classList.toggle('collapsed');
  hdr.nextElementSibling.classList.toggle('hidden');
}

async function loadTrace(){
  document.getElementById('main').innerHTML='<div class="loading">Loading…</div>';
  const res=await fetch('/api/trace');
  const data=await res.json();
  renderTrace(data);
}

loadTrace();
</script>
</body>
</html>`
