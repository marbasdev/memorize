# AI 자율 설정 가이드 (`praxis`)

[English (AI_SETUP.md)](./AI_SETUP.md) · [README로 돌아가기](../README.ko.md)

> **AI 코딩 에이전트(Claude Code, Antigravity, Cursor, Windsurf 등) 전용 런북**
> 개발자가 이 프로젝트에 `praxis` 설치·설정을 요청하면 아래 5단계를 순서대로 수행하세요.

## 개발자용 1줄 설정 프롬프트

```text
guides/AI_SETUP.ko.md 가이드를 따라 이 프로젝트에 praxis를 설치 및 설정해 줘. praxis doctor로 상태를 점검하고, 추론 엔진 상태를 확인한 뒤 초기 프로젝트 온보딩을 진행해 줘.
```
*(원격 저장소 기준 실행 시 `guides/AI_SETUP.ko.md` 대신 `https://github.com/marbasdev/praxis/blob/main/guides/AI_SETUP.ko.md` 지정)*

---

## 에이전트 실행 절차

### 1단계: 운영체제 및 클라이언트 확인
- **OS 확인**: `darwin` · `linux` · `windows` 구분 (`windows`는 내장 GGUF 미지원이므로 로컬 URL 또는 API 키 안내).
- **클라이언트 확인**: **Antigravity**(`agy` CLI), **Claude Code**(`/plugin`), 또는 **범용 MCP 클라이언트**(Cursor, Windsurf, Claude Desktop 등).

### 2단계: 설치 및 등록

- **Antigravity (`agy`)**
  ```bash
  agy plugin install ./plugins/praxis && agy plugin list
  ```
- **Claude Code**
  ```bash
  /plugin marketplace add https://github.com/marbasdev/praxis.git
  /plugin install praxis@praxis-marketplace
  ```
- **단독 바이너리 · Windows · 범용 MCP 클라이언트**
  ```bash
  CGO_ENABLED=0 go build -trimpath -o bin/praxis ./cmd/praxis
  ```
  클라이언트 MCP 설정 파일(`.cursor/mcp.json`, `claude_desktop_config.json` 등)에 등록합니다 _(macOS/Linux는 자동 동기화 래퍼 `./plugins/praxis/scripts/run-praxis.sh` 권장)_:
  ```json
  {
    "mcpServers": {
      "praxis": {
        "command": "/절대경로/praxis/bin/praxis"
      }
    }
  }
  ```

### 3단계: 자가 진단 (`praxis doctor`)
```bash
praxis doctor --json   # 사람이 읽을 때는 praxis doctor (또는 ./bin/praxis doctor)
```
점검 항목(`binary`, `daemon`, `storage`, `git_scope`, `inference`, `remote`) 중 `[WARN]` 또는 `[FAIL]`이 있으면 출력된 `hint`에 따라 조치합니다.

### 4단계: 추론 엔진 연결 확인
- `inference` 항목이 미설정(`[WARN]`)이면 기억 정제·검색·가드레일이 동작하지 않음을 알리고 다음 중 하나를 안내합니다:
  1. 웹 콘솔(**`http://127.0.0.1:18095`** → **추론 엔진** 탭)에서 **로컬 GGUF 모델 1클릭 다운로드**(macOS/Linux) 또는 로컬 URL/API 키 입력
  2. 환경변수 지정(예: `export GEMINI_API_KEY="your-api-key"`) 또는 팀 서버 연결(`praxis connect`)

### 5단계: 초기 프로젝트 온보딩
1. `query_project_memory` 및 `team_rules(action="list")`로 현재 저장소(`project:<repo>`)의 활성 규칙 유무를 확인합니다.
2. 활성 기억이 `0`개이면 개발자에게 온보딩 진행 여부를 먼저 묻습니다.
3. **수락 시** (`praxis://playbook/memory-curator` 기준):
   - 핵심 매니페스트·아키텍처·설정 파일(`go.mod` / `package.json`, `README.md`, 주요 엔트리포인트 등)을 점검합니다.
   - 코드베이스 내 옛 패턴과 새 패턴이 충돌하면 **저장 전에 어느 기준을 따를지 개발자에게 먼저 확인**합니다.
   - 확정된 규칙을 독립된 단위로 쪼개어 `commit_memory(candidate_facts=[...])`로 저장하고 `project_progress(action="record")`로 완료 이력을 남깁니다.
4. **거절 시**: `manage_memory(action="skip_onboarding")`를 호출해 반복 제안을 끕니다.
