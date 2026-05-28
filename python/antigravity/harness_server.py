# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import argparse
import asyncio
import importlib.util
import json
import logging
import os
import sys
import uuid
from fastapi import FastAPI, WebSocket, WebSocketDisconnect
import uvicorn

from google.protobuf.json_format import Parse
from proto import ax_pb2
from google.antigravity import Agent, AgentConfig
from google.antigravity.types import Step, StepType, StepSource, StepTarget, StepStatus, Text, Thought

app = FastAPI()

# Global placeholder for loaded agent config
loaded_config: AgentConfig | None = None

def load_agent_config(agent_file: str) -> AgentConfig:
    print(f"Loading agent config from {agent_file}...")
    spec = importlib.util.spec_from_file_location("agent_module", agent_file)
    if spec is None or spec.loader is None:
        raise FileNotFoundError(f"Could not find or load agent file: {agent_file}")
    agent_module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(agent_module)
    
    config = getattr(agent_module, "agent_config", None)
    if not config:
        raise ValueError(f"No 'agent_config' found in {agent_file}")
    print("Agent config loaded successfully.")
    return config

def hydrate_ax_history_to_steps(historical_messages) -> list[Step]:
    steps = []
    for i, msg in enumerate(historical_messages):
        source = StepSource.UNKNOWN
        target = StepTarget.UNSPECIFIED
        step_type = StepType.TEXT_RESPONSE
        content = ""
        thinking = ""
        
        # Determine source and target based on role
        if msg.role == "user":
            source = StepSource.USER
            target = StepTarget.ENVIRONMENT
        elif msg.role in ("assistant", "model"):
            source = StepSource.MODEL
            target = StepTarget.USER
            
        # Extract content/thinking
        active_type = msg.content.WhichOneof('type')
        if active_type == 'text':
            content = msg.content.text.text
        elif active_type == 'thought':
            step_type = StepType.TEXT_RESPONSE
            if msg.content.thought.summary:
                texts = []
                for s in msg.content.thought.summary:
                    if s.WhichOneof('type') == 'text':
                        texts.append(s.text.text)
                thinking = "".join(texts)
                
        step = Step(
            id=f"hist-{i}",
            step_index=i,
            type=step_type,
            source=source,
            target=target,
            status=StepStatus.DONE,
            content=content,
            thinking=thinking,
            is_complete_response=True
        )
        steps.append(step)
    return steps

@app.websocket("/ws")
async def websocket_endpoint(websocket: WebSocket):
    await websocket.accept()
    print("[WS] Connection accepted.")
    try:
        # 1. Receive the start message
        data = await websocket.receive_text()
        payload = json.loads(data)
        
        conversation_id = payload.get("conversation_id")
        exec_id = payload.get("exec_id")
        raw_messages = payload.get("messages", [])
        
        print(f"[WS] Starting turn. conv_id={conversation_id}, exec_id={exec_id}, messages_count={len(raw_messages)}")
        
        # Deserialize AX protobuf messages
        ax_messages = []
        for raw_msg in raw_messages:
            msg_str = json.dumps(raw_msg)
            ax_msg = Parse(msg_str, ax_pb2.Message())
            ax_messages.append(ax_msg)
            
        if not ax_messages:
            raise ValueError("No messages found in start payload")
            
        historical_messages = ax_messages[:-1]
        latest_message = ax_messages[-1]
        
        # Only support text queries for now in latest_message
        if latest_message.content.WhichOneof('type') != 'text':
            raise ValueError("Latest message must contain text content")
        latest_query_text = latest_message.content.text.text
        
        # 2. Initialize the Antigravity Agent session
        global loaded_config
        if not loaded_config:
            raise RuntimeError("Agent config is not loaded on the server")
            
        async with Agent(loaded_config) as agent:
            conversation = agent.conversation
            
            # Hydrate history
            print(f"[WS] Hydrating {len(historical_messages)} historical messages...")
            history_steps = hydrate_ax_history_to_steps(historical_messages)
            conversation._steps.extend(history_steps)
            
            # Run the turn with streaming
            print(f"[WS] Running chat query: {latest_query_text}")
            response = await conversation.chat(latest_query_text)
            
            async for chunk in response.chunks:
                if isinstance(chunk, Text):
                    await websocket.send_json({"type": "text", "content": chunk.text})
                elif isinstance(chunk, Thought):
                    await websocket.send_json({"type": "thought", "content": chunk.text})
                    
        # Send complete frame
        await websocket.send_json({"type": "complete"})
        print("[WS] Turn completed successfully.")
        
    except WebSocketDisconnect:
        print("[WS] Client disconnected.")
    except Exception as e:
        logging.exception("Error in WebSocket turn handler")
        try:
            await websocket.send_json({"type": "error", "error": str(e)})
        except Exception:
            pass
        finally:
            await websocket.close()

def main():
    parser = argparse.ArgumentParser(description="Antigravity WebSocket Harness Server")
    parser.add_argument("--agent_file", default="examples/antigravity_agent/agent.py", help="Path to the agent config file")
    parser.add_argument("--port", type=int, default=50053, help="Port to bind the server to")
    parser.add_argument("--host", default="localhost", help="Host to bind the server to")
    args = parser.parse_args()
    
    # Load the agent config globally
    global loaded_config
    try:
        loaded_config = load_agent_config(args.agent_file)
    except Exception as e:
        print(f"ERROR: Failed to load agent config: {e}", file=sys.stderr)
        sys.exit(1)
        
    print(f"Starting WebSocket server on {args.host}:{args.port}...")
    uvicorn.run(app, host=args.host, port=args.port)

if __name__ == "__main__":
    main()
