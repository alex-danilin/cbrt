---
name: triage
description: Sorts new tasky issues by area and urgency.
model: flash
mainAgent: true
subagent: true
hidden: false
inheritMcp: true
permissionMode: acceptEdits
commandExecutionPolicy: auto
tools:
  - view_file
  - grep_search
  - run_command
  - manage_task
skills:
  - issue-labels
---
Label each new issue with one area (cli, store, sync) and one urgency (now, soon, later).
Read the code the issue mentions before choosing the area.
