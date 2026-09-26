# Schedules

Recurring BE-Code schedules for this project. Each `##` section is one schedule; BE-Code runs it only while a session is open in this workspace. You may edit a schedule here, but BE-Code asks again before running one whose time, instruction, task or allowance changed.

## nightly-tests
id: 3f2a9c1b
when: weekdays 09:00
instruction: pull, run the tests
  and summarise what broke
task: 3.2
allow: shell: go test ./...
allow: write: docs
state: active
created-by: person
created: 2026-09-26T10:00:00-07:00
last-run: 2026-09-27T09:00:00-07:00
last-outcome: ok
ask-timeout: 5m0s
