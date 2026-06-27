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
# This gRPC server implements the AX HarnessService protocol. It embeds the
# Antigravity weather agent logic directly, serving it over production gRPC.

import argparse

import asyncio
import logging
import os
import sys
import grpc
from grpc_health.v1 import health, health_pb2, health_pb2_grpc
from google.protobuf.struct_pb2 import Struct

from python.proto import ax_pb2
from python.proto import ax_pb2_grpc
from python.proto import content_pb2
from google.antigravity import Agent, AgentConfig, LocalAgentConfig
from google.antigravity.types import Text, Thought, ToolCall

# 1. Define the custom weather tool
def get_weather(city: str) -> str:
    """Retrieves the current weather report for a specified city.

    Args:
        city (str): The name of the city for which to retrieve the weather report.

    Returns:
        str: Weather report status and details.
    """
    sys.stderr.write(f"\n[PYTHON TOOL get_weather executed for city: {city}]\n")
    sys.stderr.flush()
    c = city.lower()
    if "new york" in c or "nyc" in c:
        return "The weather in New York is sunny with a temperature of 25 degrees Celsius (77 degrees Fahrenheit)."
    elif "san francisco" in c or "sf" in c:
        return "The weather in San Francisco is foggy with a temperature of 16 degrees Celsius (60.8 degrees Fahrenheit)."
    else:
        return f"Weather information for '{city}' is not available."

# 2. Define the static agent config
loaded_config = LocalAgentConfig(
    system_instructions="You are a helpful agent. Use the get_weather tool to answer weather questions.",
    tools=[get_weather]
)

def _has_credentials(config: AgentConfig | None) -> bool:
    """Checks if Gemini credentials are set either in env or config."""
    # Check environment variables
    has_api_key = os.environ.get("GEMINI_API_KEY") or os.environ.get("GOOGLE_API_KEY")
    use_vertex = (
        os.environ.get("GOOGLE_GENAI_USE_VERTEXAI", "").lower() in ("true", "1") or
        os.environ.get("GOOGLE_GENAI_USE_ENTERPRISE", "").lower() in ("true", "1")
    )
    if has_api_key or use_vertex:
        return True
        
    # Check configuration
    if config:
        # Check nested gemini_config
        gemini_config = getattr(config, "gemini_config", None)
        if gemini_config:
            # 1. Direct configuration
            if getattr(gemini_config, "api_key", None) or getattr(gemini_config, "vertex", False):
                return True
            # 2. Per-model configuration
            models = getattr(gemini_config, "models", None)
            default_model = getattr(models, "default", None) if models else None
            if default_model and getattr(default_model, "api_key", None):
                return True
                
        # Check top-level config shorthands
        if getattr(config, "api_key", None) or getattr(config, "vertex", False):
            return True
            
    return False

class AntigravityHarnessServiceServicer(ax_pb2_grpc.HarnessServiceServicer):
    """Implements the ax.HarnessService protocol over gRPC."""

    def __init__(self):
        # TODO: Implement an eviction/idle-timeout policy to prevent unbounded memory growth in production.
        self._agents = {}
        self._lock = asyncio.Lock()

    async def _get_or_create_agent(self, conversation_id: str) -> Agent:
        async with self._lock:
            if conversation_id not in self._agents:
                global loaded_config
                if not loaded_config:
                    raise ValueError("Agent config is not loaded on the server")
                print(f"[gRPC] Creating new Agent instance for conv_id={conversation_id}")
                agent = Agent(loaded_config)
                await agent.__aenter__()
                self._agents[conversation_id] = agent
            return self._agents[conversation_id]

    async def cleanup(self):
        print("[gRPC] Cleaning up agent instances...")
        async with self._lock:
            for conv_id, agent in self._agents.items():
                try:
                    await agent.__aexit__(None, None, None)
                except Exception as e:
                    print(f"Error closing agent for conv_id={conv_id}: {e}")
            self._agents.clear()

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
            
        latest_message = ax_messages[-1]
        
        if latest_message.content.WhichOneof('type') != 'text':
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_FAILED, error_message="Latest message must contain text content")
            )
            return
        latest_query_text = latest_message.content.text.text
        
        # 2. Initialize or get the Antigravity Agent session
        global loaded_config
        if not loaded_config:
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_FAILED, error_message="Agent config is not loaded on the server")
            )
            return
            
        # Check credentials
        if not _has_credentials(loaded_config):
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(
                    state=ax_pb2.STATE_FAILED,
                    error_message=(
                        "No Gemini credentials configured. Please set the GEMINI_API_KEY environment variable "
                        "(AI Studio) or GOOGLE_GENAI_USE_VERTEXAI=True (Vertex AI) before starting the harness server."
                    )
                )
            )
            return
        try:
            agent = await self._get_or_create_agent(request.conversation_id)
            conversation = agent.conversation
            
            # The harness is stateful: the SDK's cached Agent (per conversation_id)
            # holds the conversation history across turns within this process
            # lifetime. The controller only sends the new turn's input; no history
            # hydration from the client side.
            print(f"[gRPC] Running chat query: {latest_query_text}")
            response = await conversation.chat(latest_query_text)
            
            # To avoid streaming individual tokens inside TextContent messages (which is not
            # supported by the Interactions proto/TextContent specifications), we buffer
            # contiguous blocks of text and thought chunks, yielding them only when the 
            # contiguous block ends or a different chunk type is received.
            text_chunks = []
            thought_chunks = []
            
            def flush_text():
                if not text_chunks:
                    return None
                msg = ax_pb2.Message(
                    role="assistant",
                    content=content_pb2.Content(text=content_pb2.TextContent(text="".join(text_chunks)))
                )
                text_chunks.clear()
                return ax_pb2.HarnessResponse(
                    conversation_id=request.conversation_id,
                    outputs=ax_pb2.HarnessOutputs(messages=[msg])
                )
                
            def flush_thought():
                if not thought_chunks:
                    return None
                summary = [
                    content_pb2.ThoughtSummaryContent(text=content_pb2.TextContent(text="".join(thought_chunks)))
                ]
                thought_chunks.clear()
                msg = ax_pb2.Message(
                    role="model",
                    content=content_pb2.Content(thought=content_pb2.ThoughtContent(summary=summary))
                )
                return ax_pb2.HarnessResponse(
                    conversation_id=request.conversation_id,
                    outputs=ax_pb2.HarnessOutputs(messages=[msg])
                )
            
            async for chunk in response.chunks:
                if isinstance(chunk, Text):
                    if (resp := flush_thought()):
                        yield resp
                    text_chunks.append(chunk.text)
                elif isinstance(chunk, Thought):
                    if (resp := flush_text()):
                        yield resp
                    thought_chunks.append(chunk.text)
                elif isinstance(chunk, ToolCall):
                    # Flush all pending text/thought buffers before dispatching the tool call
                    if (resp := flush_text()):
                        yield resp
                    if (resp := flush_thought()):
                        yield resp
                    
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
            
            # Flush any remaining text/thought buffers after the generator loop ends
            if (resp := flush_text()):
                yield resp
            if (resp := flush_thought()):
                yield resp
                        
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
    servicer = AntigravityHarnessServiceServicer()
    ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)

    # Serve the standard gRPC health protocol.
    health_servicer = health.aio.HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)
    await health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)
    
    listen_addr = f"{host}:{port}"
    server.add_insecure_port(listen_addr)
    print(f"Starting gRPC harness server on {listen_addr}...")
    await server.start()
    try:
        await server.wait_for_termination()
    finally:
        await servicer.cleanup()

def enhance_config_from_env(config) -> None:
    skills_dir = os.environ.get("SKILLS_DIR")
    if skills_dir and os.path.isdir(skills_dir):
        print(f"Adding preinstalled skills directory to agent config: {skills_dir}")
        if not hasattr(config, "skills_paths") or config.skills_paths is None:
            config.skills_paths = []
        config.skills_paths = list(config.skills_paths)
        if skills_dir not in config.skills_paths:
            config.skills_paths.append(skills_dir)


def main():
    parser = argparse.ArgumentParser(description="Antigravity gRPC Harness Server")
    parser.add_argument("--port", type=int, default=50053, help="Port to bind the server to")
    parser.add_argument("--host", default="localhost", help="Host to bind the server to")
    args = parser.parse_args()

    global loaded_config
    enhance_config_from_env(loaded_config)
        
    asyncio.run(serve(args.host, args.port))

if __name__ == "__main__":
    main()
