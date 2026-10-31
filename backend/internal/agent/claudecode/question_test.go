// Tests recorded Claude question identity, ownership, retries, and retirement through native and v3 parsing.

package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
)

const questionMetadata = `{"t":"caic_meta","version":3,"harness":"claude","prompt":"question test","repos":[],"started_at":"2026-10-30T00:00:00Z"}`
const questionRequest = `{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}}}`
const questionAnswer = `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}],"answers":{"Which?":"A"}}}}}`

func TestWireFormat(t *testing.T) {
	t.Parallel()
	t.Run("ParseInput", func(t *testing.T) {
		t.Parallel()
		t.Run("QuestionCorrelation", func(t *testing.T) {
			t.Parallel()
			for _, physical := range []bool{false, true} {
				t.Run("physical="+strconv.FormatBool(physical), func(t *testing.T) {
					t.Parallel()
					t.Run("matched request supplies questions", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, questionRequest)
						messages := parse(true, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"answers":{"Which?":"A"}}}}}`)
						assertQuestionAnswer(t, messages, &agent.MessageSource{ThreadID: "root"})
					})
					t.Run("unrelated responses remain raw", func(t *testing.T) {
						t.Parallel()
						for _, tc := range []struct {
							name    string
							request string
							input   string
						}{
							{"orphan", "", questionAnswer},
							{"permission with question shaped input", `{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"Read","tool_use_id":"ask","input":{"questions":[{"question":"Which?"}]}}}`, questionAnswer},
							{"wrong id", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"other","response":{"behavior":"allow","updatedInput":{"questions":[{"question":"Which?"}],"answers":{"Which?":"A"}}}}}`},
							{"rewritten question", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"questions":[{"question":"Injected?"}],"answers":{"Injected?":"A"}}}}}`},
							{"contradictory question echo", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"questions":[{"question":"Other?"}],"answers":{"Which?":"A"}}}}}`},
							{"incomplete", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"answers":{}}}}}`},
							{"null answer", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"answers":{"Which?":null}}}}}`},
							{"extra answer", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"answers":{"Which?":"A","Other?":"B"}}}}}`},
							{"deny", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"deny","updatedInput":{"questions":[{"question":"Which?"}],"answers":{"Which?":"A"}}}}}`},
							{"error", questionRequest, `{"type":"control_response","response":{"subtype":"error","request_id":"request","error":"closed"}}`},
							{"conflicting input owner", questionRequest, `{"type":"control_response","parent_tool_use_id":"child","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"answers":{"Which?":"A"}}}}}`},
							{"conflicting response tool", questionRequest, `{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","toolUseID":"other","updatedInput":{"answers":{"Which?":"A"}}}}}`},
						} {
							t.Run(tc.name, func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								if tc.request != "" {
									parse(false, tc.request)
								}
								assertQuestionRaw(t, parse(true, tc.input), tc.input)
							})
						}
					})
					t.Run("request lifetime", func(t *testing.T) {
						t.Parallel()
						for _, tc := range []struct {
							name string
							line string
						}{
							{"trailing result", `{"type":"result","session_id":"root","usage":{}}`},
							{"failed tool", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","is_error":true,"content":"retry"}]}}`},
							{"unrelated tool", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"other","content":"done"}]}}`},
							{"child tool", `{"type":"user","parent_tool_use_id":"spawn","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`},
							{"same root init", `{"type":"system","subtype":"init","session_id":"root"}`},
							{"child init", `{"type":"system","subtype":"init","session_id":"child","parent_tool_use_id":"spawn"}`},
						} {
							t.Run(tc.name, func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
								parse(false, questionRequest)
								parse(false, tc.line)
								assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "root"})
								// An input attempt is not a delivery outcome. A retry must
								// still have exactly the same request context.
								assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "root"})
							})
						}
					})
					t.Run("retired requests remain raw", func(t *testing.T) {
						t.Parallel()
						for _, tc := range []struct{ name, line string }{
							{"cancel", `{"type":"control_cancel_request","request_id":"request"}`},
							{"successful tool", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`},
							{"new root", `{"type":"system","subtype":"init","session_id":"new-root"}`},
						} {
							t.Run(tc.name, func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
								parse(false, questionRequest)
								parse(false, tc.line)
								assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
							})
						}
					})
					t.Run("duplicates preserve child ownership", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"assistant","session_id":"child","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						for range 2 {
							messages := parse(false, questionRequest)
							if len(messages) != 1 || messages[0].Provenance().ThreadID != "child" || messages[0].Provenance().DelegationToolUseID != "spawn" {
								t.Fatalf("duplicate request lost child owner: %#v", messages)
							}
						}
						assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "child", DelegationToolUseID: "spawn"})
					})
					t.Run("duplicate retains owner after another child ask", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"assistant","session_id":"child-a","parent_tool_use_id":"spawn-a","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						parse(false, questionRequest)
						parse(false, `{"type":"assistant","session_id":"child-b","parent_tool_use_id":"spawn-b","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						messages := parse(false, questionRequest)
						if len(messages) != 1 {
							t.Fatalf("duplicate messages = %#v", messages)
						}
						pending, ok := messages[0].(*agent.PendingUserActionMessage)
						if !ok || pending.Provenance().ThreadID != "child-a" || pending.Provenance().DelegationToolUseID != "spawn-a" {
							t.Fatalf("duplicate lost established owner: %#v", messages[0])
						}
						assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "child-a", DelegationToolUseID: "spawn-a"})
					})
					t.Run("unknown agent cannot borrow child ownership", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"assistant","session_id":"child","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						request := strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"unknown-child","tool_use_id":"ask"`, 1)
						assertQuestionRaw(t, parse(false, request), request)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
					})
					t.Run("native child cannot rewrite root request", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"assistant","session_id":"root","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						parse(false, questionRequest)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"known-child"}`)
						request := strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1)
						assertQuestionRaw(t, parse(false, request), request)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
					})
					t.Run("root completion preserves native child request", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"known-child"}`)
						request := strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1)
						parse(false, request)
						owner := &agent.MessageSource{ThreadID: "root", AgentID: "claude:task:known-child"}
						assertQuestionAnswer(t, parse(true, questionAnswer), owner)
						parse(false, `{"type":"user","session_id":"root","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
						assertQuestionAnswer(t, parse(true, questionAnswer), owner)
						parse(false, request)
						assertQuestionAnswer(t, parse(true, questionAnswer), owner)
						parse(false, `{"type":"control_cancel_request","request_id":"request"}`)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
					})
					t.Run("native child cancellation preserves root ask ownership", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"assistant","session_id":"root","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"known-child"}`)
						parse(false, strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1))
						parse(false, `{"type":"control_cancel_request","request_id":"request"}`)
						parse(false, `{"type":"assistant","session_id":"other-child","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						// Both root and child Asks still exist, so a fresh control
						// without provenance cannot choose between their owners.
						request := strings.Replace(questionRequest, `"request_id":"request"`, `"request_id":"fresh"`, 1)
						assertQuestionRaw(t, parse(false, request), request)
					})
					t.Run("native child learns delegation before completion", func(t *testing.T) {
						t.Parallel()
						for _, duplicate := range []bool{false, true} {
							t.Run("duplicate="+strconv.FormatBool(duplicate), func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
								parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"known-child"}`)
								request := strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1)
								parse(false, request)
								assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "root", AgentID: "claude:task:known-child"})
								parse(false, `{"type":"system","subtype":"task_updated","session_id":"root","task_id":"known-child","tool_use_id":"spawn"}`)
								if duplicate {
									messages := parse(false, request)
									if len(messages) != 1 {
										t.Fatalf("duplicate messages = %#v", messages)
									}
									pending, ok := messages[0].(*agent.PendingUserActionMessage)
									if !ok || pending.Provenance().AgentID != "claude:task:known-child" || pending.Provenance().DelegationToolUseID != "spawn" {
										t.Fatalf("enriched duplicate owner = %#v", messages[0])
									}
								}
								parse(false, `{"type":"user","session_id":"root","parent_tool_use_id":"spawn","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
								assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
							})
						}
					})
					t.Run("ambiguous delegation cannot complete native requests", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"child-a","tool_use_id":"shared"}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"child-b","tool_use_id":"shared"}`)
						requestA := strings.Replace(questionRequest, `"request_id":"request"`, `"request_id":"request-a"`, 1)
						requestB := strings.Replace(questionRequest, `"request_id":"request"`, `"request_id":"request-b"`, 1)
						parse(false, strings.Replace(requestA, `"tool_use_id":"ask"`, `"agent_id":"child-a","tool_use_id":"ask"`, 1))
						parse(false, strings.Replace(requestB, `"tool_use_id":"ask"`, `"agent_id":"child-b","tool_use_id":"ask"`, 1))
						answerA := strings.Replace(questionAnswer, `"request_id":"request"`, `"request_id":"request-a"`, 1)
						answerB := strings.Replace(questionAnswer, `"request_id":"request"`, `"request_id":"request-b"`, 1)
						ownerA := &agent.MessageSource{ThreadID: "root", DelegationToolUseID: "shared", AgentID: "claude:task:child-a"}
						ownerB := &agent.MessageSource{ThreadID: "root", DelegationToolUseID: "shared", AgentID: "claude:task:child-b"}
						assertQuestionAnswer(t, parse(true, answerA), ownerA)
						assertQuestionAnswer(t, parse(true, answerB), ownerB)
						parse(false, `{"type":"user","session_id":"root","parent_tool_use_id":"shared","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
						assertQuestionAnswer(t, parse(true, answerA), ownerA)
						assertQuestionAnswer(t, parse(true, answerB), ownerB)
						parse(false, requestA)
						assertQuestionAnswer(t, parse(true, answerA), ownerA)
						parse(false, `{"type":"user","session_id":"root","parent_tool_use_id":"shared","agent_id":"child-a","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
						assertQuestionRaw(t, parse(true, answerA), answerA)
						assertQuestionAnswer(t, parse(true, answerB), ownerB)
						parse(false, `{"type":"user","session_id":"root","parent_tool_use_id":"shared","agent_id":"unknown","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
						parse(false, `{"type":"user","session_id":"root","agent_id":"child-b","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
						assertQuestionAnswer(t, parse(true, answerB), ownerB)
						parse(false, `{"type":"user","session_id":"root","parent_tool_use_id":"shared","agent_id":"child-b","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
						assertQuestionRaw(t, parse(true, answerB), answerB)
					})
					t.Run("ambiguous delegation cannot establish question owner", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"child-a","tool_use_id":"shared"}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"child-b","tool_use_id":"shared"}`)
						parse(false, `{"type":"assistant","session_id":"root","parent_tool_use_id":"shared","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						request := strings.Replace(questionRequest, `"type":"control_request"`, `"type":"control_request","parent_tool_use_id":"shared"`, 1)
						assertQuestionRaw(t, parse(false, request), request)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
						fresh := strings.Replace(questionRequest, `"request_id":"request"`, `"request_id":"fresh"`, 1)
						assertQuestionRaw(t, parse(false, fresh), fresh)
					})
					t.Run("explicit scope excludes unrelated ambiguous asks", func(t *testing.T) {
						t.Parallel()
						for _, tc := range []struct{ name, thread, delegation string }{
							{"delegation", "root", "safe"},
							{"thread", "safe-thread", "shared"},
						} {
							t.Run(tc.name, func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
								parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"child-a","tool_use_id":"shared"}`)
								parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"child-b","tool_use_id":"shared"}`)
								parse(false, `{"type":"assistant","session_id":"root","parent_tool_use_id":"shared","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
								parse(false, `{"type":"assistant","session_id":"`+tc.thread+`","parent_tool_use_id":"`+tc.delegation+`","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
								request := strings.Replace(questionRequest, `"type":"control_request"`, `"type":"control_request","session_id":"`+tc.thread+`","parent_tool_use_id":"`+tc.delegation+`"`, 1)
								parse(false, request)
								assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: tc.thread, DelegationToolUseID: tc.delegation})
							})
						}
					})
					t.Run("known native owner metadata cannot conflict", func(t *testing.T) {
						t.Parallel()
						for _, tc := range []struct{ name, metadata, request string }{
							{"delegation", `{"type":"system","subtype":"task_updated","session_id":"root","task_id":"known-child","tool_use_id":"other-spawn"}`, strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1)},
							{"native id", `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"other-child","tool_use_id":"spawn"}`, strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"other-child","tool_use_id":"ask"`, 1)},
							{"thread", "", strings.Replace(questionRequest, `"type":"control_request"`, `"type":"control_request","session_id":"other-thread"`, 1)},
						} {
							t.Run(tc.name, func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
								parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"known-child","tool_use_id":"spawn"}`)
								parse(false, strings.Replace(questionRequest, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1))
								if tc.metadata != "" {
									parse(false, tc.metadata)
								}
								assertQuestionRaw(t, parse(false, tc.request), tc.request)
								assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
							})
						}
					})
					t.Run("reported native identity preserves explicit delegation", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"known-child"}`)
						request := strings.Replace(questionRequest, `"type":"control_request"`, `"type":"control_request","parent_tool_use_id":"spawn"`, 1)
						request = strings.Replace(request, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1)
						parse(false, request)
						assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "root", AgentID: "claude:task:known-child", DelegationToolUseID: "spawn"})
						parse(false, `{"type":"system","subtype":"task_updated","session_id":"root","task_id":"known-child","tool_use_id":"other-spawn"}`)
						assertQuestionRaw(t, parse(false, request), request)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
					})
					t.Run("reported native identity cannot replace conflicting delegation", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"system","subtype":"task_started","session_id":"root","task_type":"local_agent","task_id":"known-child","tool_use_id":"other-spawn"}`)
						request := strings.Replace(questionRequest, `"type":"control_request"`, `"type":"control_request","parent_tool_use_id":"spawn"`, 1)
						request = strings.Replace(request, `"tool_use_id":"ask"`, `"agent_id":"known-child","tool_use_id":"ask"`, 1)
						assertQuestionRaw(t, parse(false, request), request)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
					})
					t.Run("empty string is a complete answer", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, questionRequest)
						messages := parse(true, strings.Replace(questionAnswer, `"Which?":"A"`, `"Which?":""`, 1))
						if len(messages) != 1 {
							t.Fatalf("empty answer messages = %#v", messages)
						}
						if answer, ok := messages[0].(*agent.UserInputMessage); !ok || answer.Text != "" {
							t.Fatalf("empty string answer = %#v", messages[0])
						}
					})
					t.Run("conflicting reuse stays ineligible", func(t *testing.T) {
						t.Parallel()
						for _, line := range []string{
							`{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"Read","input":{}}}`,
							`{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"other","input":{"questions":[{"question":"Which?"}]}}}`,
							`{"type":"control_request","parent_tool_use_id":"child","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":{"questions":[{"question":"Which?"}]}}}`,
						} {
							t.Run(line, func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								parse(false, questionRequest)
								parse(false, line)
								parse(false, questionRequest)
								assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
							})
						}
					})
					t.Run("root and child requests keep separate owners", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, questionRequest)
						parse(false, `{"type":"assistant","session_id":"child","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						parse(false, `{"type":"control_request","request_id":"child-request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":{"questions":[{"question":"Which?"}]}}}`)
						assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "root"})
						childAnswer := `{"type":"control_response","response":{"subtype":"success","request_id":"child-request","response":{"behavior":"allow","updatedInput":{"answers":{"Which?":"A"}}}}}`
						assertQuestionAnswer(t, parse(true, childAnswer), &agent.MessageSource{ThreadID: "child", DelegationToolUseID: "spawn"})
						parse(false, `{"type":"user","session_id":"child","parent_tool_use_id":"spawn","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"ask","content":"done"}]}}`)
						assertQuestionRaw(t, parse(true, childAnswer), childAnswer)
						assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "root"})
					})
					t.Run("ambiguous request is not answer context", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						parse(false, `{"type":"assistant","parent_tool_use_id":"child","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						parse(false, questionRequest)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
					})
					t.Run("permission id cannot be repurposed", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"Read","tool_use_id":"ask","input":{}}}`)
						parse(false, questionRequest)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
					})
					t.Run("invalid request metadata remains informational", func(t *testing.T) {
						t.Parallel()
						for _, tc := range []struct{ name, request string }{
							{"missing id", `{"type":"control_request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":{"questions":[{"question":"Which?"}]}}}`},
							{"missing tool", `{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}}`},
							{"empty question", `{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":{"questions":[{"question":""}]}}}`},
							{"duplicate question", `{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"ask","input":{"questions":[{"question":"Which?"},{"question":"Which?"}]}}}`},
							{"unknown reported owner", `{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","agent_id":"unknown-child","tool_use_id":"ask","input":{"questions":[{"question":"Which?"}]}}}`},
						} {
							t.Run(tc.name, func(t *testing.T) {
								t.Parallel()
								parse := questionParser(t, physical)
								parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
								assertQuestionRaw(t, parse(false, tc.request), tc.request)
								assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
							})
						}
					})
					t.Run("new session owns reused request id", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, questionRequest)
						parse(false, `{"type":"system","subtype":"init","session_id":"new-root"}`)
						parse(false, questionRequest)
						assertQuestionAnswer(t, parse(true, questionAnswer), &agent.MessageSource{ThreadID: "new-root"})
					})
					t.Run("cancel preserves another request for the same child tool", func(t *testing.T) {
						t.Parallel()
						parse := questionParser(t, physical)
						parse(false, `{"type":"system","subtype":"init","session_id":"root"}`)
						parse(false, `{"type":"assistant","session_id":"child","parent_tool_use_id":"spawn","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?"}]}}]}}`)
						parse(false, questionRequest)
						otherRequest := strings.ReplaceAll(questionRequest, `"request_id":"request"`, `"request_id":"other"`)
						parse(false, otherRequest)
						parse(false, `{"type":"control_cancel_request","request_id":"request"}`)
						parse(false, otherRequest)
						otherAnswer := strings.ReplaceAll(questionAnswer, `"request_id":"request"`, `"request_id":"other"`)
						assertQuestionRaw(t, parse(true, questionAnswer), questionAnswer)
						assertQuestionAnswer(t, parse(true, otherAnswer), &agent.MessageSource{ThreadID: "child", DelegationToolUseID: "spawn"})
					})
				})
			}
		})
		t.Run("RecordedQuestionEvidence", func(t *testing.T) {
			t.Parallel()
			f, err := os.Open("testdata/ask-user-question.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := f.Close(); err != nil {
					t.Error(err)
				}
			})
			parser, err := agent.NewLogRecordParser(New().NewWire())
			if err != nil {
				t.Fatal(err)
			}
			answers := 0
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
					continue
				}
				record, err := parser.ParseRecord(scanner.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				for _, message := range record.Messages {
					if input, ok := message.Message.(*agent.UserInputMessage); ok && input.Text == "Hi" {
						answers++
						if input.Provenance().ThreadID != "test-session" || input.Provenance().IsChild() {
							t.Fatalf("recorded answer owner = %+v", input.Provenance())
						}
					}
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if answers != 1 {
				t.Fatalf("recorded Hi answers = %d, want one", answers)
			}
		})
		t.Run("QuestionReconnectAfterAttempt", func(t *testing.T) {
			t.Parallel()
			for _, outcome := range []struct {
				name    string
				message agent.Message
			}{
				{"failed", agent.InputDeliveryFailed(io.ErrClosedPipe)},
				{"interrupted", nil},
			} {
				t.Run(outcome.name, func(t *testing.T) {
					t.Parallel()
					lines := []string{
						questionMetadata,
						`{"t":"agent","ts":1.000,"msg":{"type":"system","subtype":"init","session_id":"root"}}`,
						`{"t":"agent","ts":1.000,"msg":{"type":"assistant","message":{"content":[{"type":"tool_use","id":"ask","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}}]}}}`,
						`{"t":"agent","ts":1.000,"msg":` + questionRequest + `}`,
						`{"t":"agent","ts":1.000,"msg":{"type":"result","usage":{}}}`,
					}
					started, err := agent.MarshalLogMessage(agent.InputDeliveryStarted())
					if err != nil {
						t.Fatal(err)
					}
					lines = append(lines, string(started), `{"t":"input","ts":1.000,"msg":`+questionAnswer+`}`)
					if outcome.message != nil {
						data, err := agent.MarshalLogMessage(outcome.message)
						if err != nil {
							t.Fatal(err)
						}
						lines = append(lines, string(data))
					}
					parser, err := agent.NewLogRecordParser(New().NewWire())
					if err != nil {
						t.Fatal(err)
					}
					var history []agent.Message
					appendRecord := func(line string) {
						record, err := parser.ParseRecord([]byte(line))
						if err != nil {
							t.Fatal(err)
						}
						for _, message := range record.Messages {
							history = append(history, message.Message)
						}
					}
					for _, line := range lines {
						appendRecord(line)
					}
					tk, err := taskrun.NewTask(ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "", "", "", "")
					if err != nil {
						t.Fatal(err)
					}
					tk.SeedTimelineParts(nil, agenttest.TimedMessages(history))
					pending := tk.PendingUserActions()
					if len(pending) != 1 || pending[0].RequestID != "request" {
						t.Fatalf("attempt closed pending Ask: %#v", pending)
					}
					// The production reconnect wrapper restores the live write selection
					// separately from the wire's replayed interpretation context.
					inner := &fakeConn{}
					connection := &controlConn{Conn: inner}
					if err := connection.restorePendingActions(pending); err != nil {
						t.Fatal(err)
					}
					if err := connection.SendPrompt(agent.Prompt{Text: "A"}, nil); err != nil {
						t.Fatal(err)
					}
					if len(inner.sent) != 1 || len(inner.prompts) != 0 {
						t.Fatalf("retry routing: controls=%q prompts=%#v", inner.sent, inner.prompts)
					}
					appendRecord(string(started))
					appendRecord(`{"t":"input","ts":1.000,"msg":` + string(bytes.TrimSpace(inner.sent[0])) + `}`)
					completed, err := agent.MarshalLogMessage(agent.InputDeliveryCompleted())
					if err != nil {
						t.Fatal(err)
					}
					appendRecord(string(completed))
					restored, err := taskrun.NewTask(tk.ID, agent.Prompt{Text: "test"}, harness.Claude, "", "", "", "", "")
					if err != nil {
						t.Fatal(err)
					}
					restored.SeedTimelineParts(nil, agenttest.TimedMessages(history))
					if pending := restored.PendingUserActions(); len(pending) != 0 {
						t.Fatalf("completed retry did not close Ask: %#v", pending)
					}
					answers := 0
					for _, message := range history {
						if input, ok := message.(*agent.UserInputMessage); ok && input.Text == "A" {
							answers++
						}
					}
					if answers != 2 {
						t.Fatalf("recovered answer attempts = %d, want two", answers)
					}
				})
			}
		})
	})
}

func questionParser(t *testing.T, physical bool) func(bool, string) []agent.Message {
	wire := New().NewWire()
	parser, err := agent.NewLogRecordParser(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseRecord([]byte(questionMetadata)); err != nil {
		t.Fatal(err)
	}
	return func(input bool, line string) []agent.Message {
		if !physical {
			parse := wire.ParseOutput
			if input {
				parse = wire.ParseInput
			}
			messages, err := parse([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			return messages
		}
		token := "agent"
		if input {
			token = "input"
		}
		record, err := parser.ParseRecord([]byte(`{"t":"` + token + `","ts":1.000,"msg":` + line + `}`))
		if err != nil {
			t.Fatal(err)
		}
		var messages []agent.Message
		for _, timed := range record.Messages {
			messages = append(messages, timed.Message)
		}
		return messages
	}
}

func assertQuestionAnswer(t *testing.T, messages []agent.Message, source *agent.MessageSource) {
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one answer", messages)
	}
	input, ok := messages[0].(*agent.UserInputMessage)
	if !ok || input.Text != "A" || !reflect.DeepEqual(input.Provenance(), source) {
		t.Fatalf("answer = %#v, want A from %+v", messages[0], source)
	}
}

func assertQuestionRaw(t *testing.T, messages []agent.Message, line string) {
	if len(messages) != 1 {
		t.Fatalf("messages = %#v, want one raw response", messages)
	}
	raw, ok := messages[0].(*agent.RawMessage)
	if !ok || !json.Valid(raw.Raw) || string(raw.Raw) != line {
		t.Fatalf("message = %#v, want unchanged raw response", messages[0])
	}
}
