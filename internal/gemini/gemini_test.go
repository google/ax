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

package gemini

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNewSkillsTool_NonExistentDir verifies that NewSkillsTool returns a NoopTool
// and no error when the specified skills directory does not exist.
func TestNewSkillsTool_NonExistentDir(t *testing.T) {
	// Use a directory that definitely does not exist
	nonExistentDir := filepath.Join(t.TempDir(), "does-not-exist")

	tool, err := NewSkillsTool(nonExistentDir)
	if err != nil {
		t.Fatalf("NewSkillsTool failed for non-existent dir: %v", err)
	}

	// Verify it returned a NoopTool
	if _, ok := tool.(*NoopTool); !ok {
		t.Errorf("Expected NoopTool, got %T", tool)
	}
	if len(tool.FuncDecl()) != 0 {
		t.Errorf("Expected empty FuncDecl for NoopTool, got %d", len(tool.FuncDecl()))
	}
	if tool.SystemPrompt() != "" {
		t.Errorf("Expected empty SystemPrompt for NoopTool, got %s", tool.SystemPrompt())
	}
}

// TestNewSkillsTool_EmptyDir verifies that NewSkillsTool returns a NoopTool
// and no error when the specified skills directory contains no skills.
func TestNewSkillsTool_EmptyDir(t *testing.T) {
	// Use an empty directory
	emptyDir := t.TempDir()

	tool, err := NewSkillsTool(emptyDir)
	if err != nil {
		t.Fatalf("NewSkillsTool failed for empty dir: %v", err)
	}

	// Verify it returned a NoopTool
	if _, ok := tool.(*NoopTool); !ok {
		t.Errorf("Expected NoopTool, got %T", tool)
	}
	if len(tool.FuncDecl()) != 0 {
		t.Errorf("Expected empty FuncDecl for NoopTool, got %d", len(tool.FuncDecl()))
	}
	if tool.SystemPrompt() != "" {
		t.Errorf("Expected empty SystemPrompt for NoopTool, got %s", tool.SystemPrompt())
	}
}

// TestNewSkillsTool_WithSkills verifies that NewSkillsTool successfully creates
// a SkillsTool when valid skills are present.
func TestNewSkillsTool_WithSkills(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := filepath.Join(tmpDir, "test-skill")
	err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	skillContent := `---
name: test-skill
description: A test skill
---
Instructions go here.`

	err = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillContent), 0644)
	if err != nil {
		t.Fatal(err)
	}

	tool, err := NewSkillsTool(tmpDir)
	if err != nil {
		t.Fatalf("NewSkillsTool failed: %v", err)
	}

	// Verify it returned a SkillsTool (FuncDecl should not be empty)
	if len(tool.FuncDecl()) == 0 {
		t.Error("Expected non-empty FuncDecl for SkillsTool")
	}
}

// TestNewSkillsTool_NoDir verifies that NewSkillsTool returns a NoopTool
// when both the specified directory and the SKILLS_DIR environment variable are empty.
func TestNewSkillsTool_NoDir(t *testing.T) {
	t.Setenv("SKILLS_DIR", "")

	tool, err := NewSkillsTool("")
	if err != nil {
		t.Fatalf("NewSkillsTool failed for empty dir: %v", err)
	}

	// Verify it returned a NoopTool
	if _, ok := tool.(*NoopTool); !ok {
		t.Errorf("Expected NoopTool, got %T", tool)
	}
}
