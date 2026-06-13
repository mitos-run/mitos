package vsock

import (
	"encoding/json"
	"testing"
)

func TestRunCodeRequestRoundTrip(t *testing.T) {
	req := Request{
		Type: TypeRunCode,
		RunCode: &RunCodeRequest{
			Code:     "print(1)",
			Language: "python",
			Timeout:  30,
		},
	}
	b, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != TypeRunCode {
		t.Fatalf("type = %q, want %q", got.Type, TypeRunCode)
	}
	if got.RunCode == nil || got.RunCode.Code != "print(1)" || got.RunCode.Language != "python" {
		t.Fatalf("run_code payload not preserved: %+v", got.RunCode)
	}
}

func TestExecStreamFrameResultAndError(t *testing.T) {
	resFrame := ExecStreamFrame{
		Kind: FrameResult,
		Result: &ResultFrame{
			Text: "42",
			Data: map[string]string{"image/png": "aGVsbG8=", "text/plain": "42"},
		},
	}
	b, err := json.Marshal(&resFrame)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var gotRes ExecStreamFrame
	if err := json.Unmarshal(b, &gotRes); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if gotRes.Result == nil || gotRes.Result.Data["image/png"] != "aGVsbG8=" || gotRes.Result.Text != "42" {
		t.Fatalf("result frame not preserved: %+v", gotRes.Result)
	}

	errFrame := ExecStreamFrame{
		Kind: FrameError,
		ErrorInfo: &ErrorFrame{
			Name:      "ValueError",
			Value:     "bad",
			Traceback: []string{"Traceback (most recent call last):", "ValueError: bad"},
		},
	}
	b, err = json.Marshal(&errFrame)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var gotErr ExecStreamFrame
	if err := json.Unmarshal(b, &gotErr); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if gotErr.ErrorInfo == nil || gotErr.ErrorInfo.Name != "ValueError" || len(gotErr.ErrorInfo.Traceback) != 2 {
		t.Fatalf("error frame not preserved: %+v", gotErr.ErrorInfo)
	}
}
