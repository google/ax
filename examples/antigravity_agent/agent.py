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
from google.antigravity.types import Text, Thought

# Expose agent_config globally for harness_server.py config loading
agent_config = LocalAgentConfig(
    system_instructions="You are a helpful assistant powered by Google Antigravity."
)

# Expose the L2 configuration strategy for custom loaders if needed
strategy_factory = lambda: LocalConnectionStrategy(tool_runner=ToolRunner())

async def main():
    # 1. Initialize the local connection strategy
    strategy = strategy_factory()
    
    # 2. Create the stateful conversation session
    print("Starting stateful Antigravity conversation (L2 API)...")
    async with Conversation.create(strategy) as conversation:
        prompt = sys.argv[1] if len(sys.argv) > 1 else "Explain quantum computing in one sentence."
        
        # 3. Send query and receive streaming ChatResponse
        response = await conversation.chat(prompt)
        
        # 4. Stream semantic chunks (Thoughts and Text) in real-time
        async for chunk in response.chunks:
            if isinstance(chunk, Text):
                sys.stdout.write(chunk.text)
                sys.stdout.flush()
            elif isinstance(chunk, Thought):
                # Display thought process in comment style
                sys.stdout.write(f"\n[Thinking]: {chunk.text}")
                sys.stdout.flush()
        print()

if __name__ == "__main__":
    asyncio.run(main())

if __name__ == "__main__":
    asyncio.run(main())
