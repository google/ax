# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# NOTE ON ARCHITECTURE:
# This file plays a dual-purpose role:
# 1. Standalone Sandbox: Can be run directly via CLI (python agent.py "prompt") for local L2 debugging.
# 2. Declarative Config Module: Exposes 'agent_config' globally, which python/antigravity/harness_server.py 
#    dynamically imports to serve this agent over production gRPC.

import asyncio

import sys
from google.antigravity import LocalAgentConfig
from google.antigravity.connections.local import LocalConnectionStrategy
from google.antigravity.conversation.conversation import Conversation
from google.antigravity.tools.tool_runner import ToolRunner
from google.antigravity.types import Text, Thought, ToolCall

# 1. Define a custom local python tool
def get_weather(city: str) -> str:
    """Retrieves the current weather report for a specified city.

    Args:
        city (str): The name of the city for which to retrieve the weather report.

    Returns:
        str: Weather report status and details.
    """
    # Output to stderr so it does not pollute the stdout stream capture
    sys.stderr.write(f"\n[PYTHON TOOL get_weather executed for city: {city}]\n")
    sys.stderr.flush()
    c = city.lower()
    if "new york" in c or "nyc" in c:
        return "The weather in New York is sunny with a temperature of 25 degrees Celsius (77 degrees Fahrenheit)."
    elif "san francisco" in c or "sf" in c:
        return "The weather in San Francisco is foggy with a temperature of 16 degrees Celsius (60.8 degrees Fahrenheit)."
    else:
        return f"Weather information for '{city}' is not available."

# 2. Expose agent_config globally for harness_server.py config loading
agent_config = LocalAgentConfig(
    system_instructions="You are a helpful agent. Use the get_weather tool to answer weather questions.",
    tools=[get_weather]
)

# Expose the L2 configuration strategy factory for custom loaders
strategy_factory = lambda: LocalConnectionStrategy(tool_runner=ToolRunner(tools=[get_weather]))

async def main():
    # 3. Initialize the local connection strategy
    strategy = strategy_factory()
    
    # 4. Create the stateful conversation session
    print("Starting stateful Antigravity conversation (L2 API)...")
    async with Conversation.create(strategy) as conversation:
        prompt = sys.argv[1] if len(sys.argv) > 1 else None
        if not prompt:
            raise ValueError("Please provide a prompt for your agent. Usage: python agent.py <prompt>")
        
        # 5. Send query and receive streaming ChatResponse
        response = await conversation.chat(prompt)
        
        # 6. Stream semantic chunks (Thoughts, Text, and ToolCalls) in real-time
        async for chunk in response.chunks:
            if isinstance(chunk, Text):
                sys.stdout.write(chunk.text)
                sys.stdout.flush()
            elif isinstance(chunk, Thought):
                # Display thought process in comment style
                sys.stdout.write(f"\n[Thinking]: {chunk.text}")
                sys.stdout.flush()
            elif isinstance(chunk, ToolCall):
                sys.stdout.write(f"\n[Tool Call]: {chunk.name} with args {chunk.args}\n")
                sys.stdout.flush()
        print()

if __name__ == "__main__":
    asyncio.run(main())


