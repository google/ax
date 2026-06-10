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

# NOTE ON ARCHITECTURE:
# This is a generic, reusable gRPC server that does not define tools or personas. 
# Instead, it dynamically imports any agent configuration file (defaulting to examples/antigravity_agent/agent.py) 
# passed via the --agent_file CLI argument, then hosts it over the AX HarnessService protocol.

import argparse

import asyncio
import importlib.util
import logging
import sys
import grpc
from grpc_health.v1 import health, health_pb2, health_pb2_grpc
from google.protobuf.struct_pb2 import Struct

from python.proto import ax_pb2
from python.proto import ax_pb2_grpc
from python.proto import content_pb2
from google.antigravity import Agent, AgentConfig
from google.antigravity.types import Step, StepType, StepSource, StepTarget, StepStatus, Text, Thought, ToolCall

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

class AntigravityHarnessServiceServicer(ax_pb2_grpc.HarnessServiceServicer):
    """Implements the ax.HarnessService protocol over gRPC."""

    async def Connect(self, request_iterator, context):
        # Each HarnessRequest{start} drives one stateless turn; the stream stays
        # open across turns until the client half-closes.
        async for request in request_iterator:
            if request.WhichOneof("type") != "start":
                continue  # cancel frames not handled yet
            async for response in self._run_turn(request):
                yield response

    async def _run_turn(self, request):
        print(f"[gRPC] Connect turn requested. conv_id={request.conversation_id}")
        
        # 1. Retrieve and check messages
        ax_messages = request.start.messages
        if not ax_messages:
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_FAILED, error_message="No messages found in start payload")
            )
            return
            
        historical_messages = ax_messages[:-1]
        latest_message = ax_messages[-1]
        
        if latest_message.content.WhichOneof('type') != 'text':
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_FAILED, error_message="Latest message must contain text content")
            )
            return
        latest_query_text = latest_message.content.text.text
        
        # 2. Initialize the Antigravity Agent session
        global loaded_config
        if not loaded_config:
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_FAILED, error_message="Agent config is not loaded on the server")
            )
            return
            
        try:
            async with Agent(loaded_config) as agent:
                conversation = agent.conversation
                
                # Hydrate history
                print(f"[gRPC] Hydrating {len(historical_messages)} historical messages...")
                history_steps = hydrate_ax_history_to_steps(historical_messages)
                conversation._steps.extend(history_steps)
                
                # Run the turn with streaming
                print(f"[gRPC] Running chat query: {latest_query_text}")
                response = await conversation.chat(latest_query_text)
                
                async for chunk in response.chunks:
                    if isinstance(chunk, Text):
                        msg = ax_pb2.Message(
                            role="assistant",
                            content=content_pb2.Content(text=content_pb2.TextContent(text=chunk.text))
                        )
                        yield ax_pb2.HarnessResponse(
                            conversation_id=request.conversation_id,
                            outputs=ax_pb2.HarnessOutputs(messages=[msg])
                        )
                    elif isinstance(chunk, Thought):
                        summary = [
                            content_pb2.ThoughtSummaryContent(text=content_pb2.TextContent(text=chunk.text))
                        ]
                        msg = ax_pb2.Message(
                            role="model",
                            content=content_pb2.Content(thought=content_pb2.ThoughtContent(summary=summary))
                        )
                        yield ax_pb2.HarnessResponse(
                            conversation_id=request.conversation_id,
                            outputs=ax_pb2.HarnessOutputs(messages=[msg])
                        )
                    elif isinstance(chunk, ToolCall):
                        struct_args = Struct()
                        struct_args.update(chunk.args)
                        
                        func_call = content_pb2.FunctionCallContent(
                            name=str(chunk.name),
                            arguments=struct_args
                        )
                        msg = ax_pb2.Message(
                            role="model",
                            content=content_pb2.Content(tool_call=content_pb2.ToolCallContent(
                                id=chunk.id or "",
                                function_call=func_call
                            ))
                        )
                        yield ax_pb2.HarnessResponse(
                            conversation_id=request.conversation_id,
                            outputs=ax_pb2.HarnessOutputs(messages=[msg])
                        )
                        
            # Yield completion end frame
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_COMPLETED)
            )
            print("[gRPC] Turn completed successfully.")
            
        except Exception as e:
            logging.exception("Error inside Connect servicer execution")
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_FAILED, error_message=f"Agent execution terminated due to error. ({str(e)})")
            )
            return

async def serve(host: str, port: int):
    server = grpc.aio.server()
    ax_pb2_grpc.add_HarnessServiceServicer_to_server(AntigravityHarnessServiceServicer(), server)

    # Serve the standard gRPC health protocol.
    health_servicer = health.aio.HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)
    await health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)
    
    listen_addr = f"{host}:{port}"
    server.add_insecure_port(listen_addr)
    print(f"Starting gRPC harness server on {listen_addr}...")
    await server.start()
    await server.wait_for_termination()

def main():
    parser = argparse.ArgumentParser(description="Antigravity gRPC Harness Server")
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
        
    asyncio.run(serve(args.host, args.port))

if __name__ == "__main__":
    main()
