package templatebuild

import (
	"testing"

	v1 "mitos.run/mitos/api/v1"
)

// TestParseDockerfileMapsInstructionsToSpec asserts a minimal Dockerfile maps to
// a template spec: FROM becomes the base image, RUN/ENV/WORKDIR/COPY become
// ordered build steps, and CMD/ENTRYPOINT become the start command.
func TestParseDockerfileMapsInstructionsToSpec(t *testing.T) {
	df := `
# a comment
FROM python:3.12-slim
WORKDIR /app
COPY app/ /app
ENV PORT 8080
RUN pip install -r requirements.txt
CMD ["python", "app.py"]
`
	spec, err := ParseDockerfile(df)
	if err != nil {
		t.Fatalf("ParseDockerfile: %v", err)
	}
	if spec.Image != "python:3.12-slim" {
		t.Errorf("image = %q, want python:3.12-slim", spec.Image)
	}
	if len(spec.BuildSteps) != 4 {
		t.Fatalf("got %d build steps, want 4: %+v", len(spec.BuildSteps), spec.BuildSteps)
	}
	if spec.BuildSteps[0].Type != v1.BuildStepWorkdir || spec.BuildSteps[0].Workdir != "/app" {
		t.Errorf("step 0 = %+v, want workdir /app", spec.BuildSteps[0])
	}
	if spec.BuildSteps[1].Type != v1.BuildStepCopy || spec.BuildSteps[1].Source != "app/" || spec.BuildSteps[1].Dest != "/app" {
		t.Errorf("step 1 = %+v, want copy app/ -> /app", spec.BuildSteps[1])
	}
	if spec.BuildSteps[2].Type != v1.BuildStepEnv || spec.BuildSteps[2].EnvName != "PORT" || spec.BuildSteps[2].EnvValue != "8080" {
		t.Errorf("step 2 = %+v, want env PORT=8080", spec.BuildSteps[2])
	}
	if spec.BuildSteps[3].Type != v1.BuildStepRun || spec.BuildSteps[3].Run != "pip install -r requirements.txt" {
		t.Errorf("step 3 = %+v, want run", spec.BuildSteps[3])
	}
	if len(spec.Command) != 2 || spec.Command[0] != "python" || spec.Command[1] != "app.py" {
		t.Errorf("command = %v, want [python app.py]", spec.Command)
	}
}

// TestParseDockerfileRequiresFrom asserts a Dockerfile without FROM is rejected.
func TestParseDockerfileRequiresFrom(t *testing.T) {
	if _, err := ParseDockerfile("RUN echo hi\n"); err == nil {
		t.Fatal("expected an error for a Dockerfile with no FROM")
	}
}

// TestParseDockerfileEnvKeyValueForm asserts ENV KEY=VALUE form parses too.
func TestParseDockerfileEnvKeyValueForm(t *testing.T) {
	spec, err := ParseDockerfile("FROM alpine\nENV FOO=bar\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.BuildSteps) != 1 || spec.BuildSteps[0].EnvName != "FOO" || spec.BuildSteps[0].EnvValue != "bar" {
		t.Fatalf("env step = %+v", spec.BuildSteps)
	}
}
