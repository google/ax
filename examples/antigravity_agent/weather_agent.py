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

import asyncio
import sys
from google.antigravity import LocalAgentConfig
from google.antigravity.connections.local import LocalConnectionStrategy
from google.antigravity.conversation.conversation import Conversation
from google.antigravity.tools.tool_runner import ToolRunner
from google.antigravity.types import Text, Thought, ToolCall

# Define the local python tool
def get_weather(city: str) -> str:
    """Retrieves the current weather report for a specified city.

    Args:
        city (str): The name of the city for which to retrieve the weather report.

    Returns:
        str: Weather report status and details.
    """
    # Output directly to stderr to not pollute the clean stream capture of stdout
    sys.stderr.write(f"\n[PYTHON TOOL get_weather executed for city: {city}]\n")
    sys.stderr.flush()
    c = city.lower()
    if "new york" in c or "nyc" in c:
        return "The weather in New York is sunny with a temperature of 25 degrees Celsius (77 degrees Fahrenheit)."
    elif "san francisco" in c or "sf" in c:
        return "The weather in San Francisco is foggy with a temperature of 16 degrees Celsius (60.8 degrees Fahrenheit)."
    else:
        return f"Weather information for '{city}' is not available."

# Expose agent_config globally for harness_server.py config loading
agent_config = LocalAgentConfig(
    system_instructions="You are a helpful weather agent. Use the get_weather tool to answer weather questions.",
    tools=[get_weather]
)

async def main():
    # 1. Initialize local connection strategy with local tool
    tool_runner = ToolRunner(tools=[get_weather])
    strategy = LocalConnectionStrategy(tool_runner=tool_runner)
    
    # 2. Create stateful conversation
    print("Starting stateful Antigravity Weather Agent (L2 API)...")
    async with Conversation.create(strategy) as conversation:
        prompt = sys.argv[1] if len(sys.argv) > 1 else "What is the weather in New York?"
        
        # 3. Execute chat query
        response = await conversation.chat(prompt)
        
        # 4. Stream chunks (Thought, Text, and ToolCall) in real-time
        async for chunk in response.chunks:
            if isinstance(chunk, Text):
                sys.stdout.write(chunk.text)
                sys.stdout.flush()
            elif isinstance(chunk, Thought):
                sys.stdout.write(f"\n[Thinking]: {chunk.text}")
                sys.stdout.flush()
            elif isinstance(chunk, ToolCall):
                sys.stdout.write(f"\n[Tool Call]: {chunk.name} with args {chunk.args}\n")
                sys.stdout.flush()
        print()

if __name__ == "__main__":
    asyncio.run(main())
