# AI 자율 설정 가이드 (`memorize`)

> **AI 코딩 에이전트(Claude Code, Antigravity, Cursor, Windsurf 등) 전용**
> 개발자가 "이 프로젝트에 memorize를 설치하고 설정해 줘"라고 요청하면, 이 가이드에 명시된 자율 실행 절차를 따르세요.

---

## 개발자용 1줄 설정 프롬프트

개발자는 아래 프롬프트 중 하나를 복사하여 에이전트 채팅에 붙여넣을 수 있습니다:

```text
guides/AI_SETUP.ko.md 가이드를 따라 이 프로젝트에 memorize를 설치 및 설정해 줘. memorize doctor로 상태를 점검하고, 추론 엔진 상태를 확인한 뒤 초기 프로젝트 온보딩을 진행해 줘.
```

*(또는 웹 저장소 URL 지정: `https://github.com/marbasdev/memorize/blob/main/guides/AI_SETUP.ko.md`)*

---

## 에이전트 자율 설정 플레이북

아래 5단계를 순서대로 실행하세요:

### 1단계: 환경 및 클라이언트 탐색

1. **운영체제(OS)**: `darwin`, `linux`, `windows` 확인.
   - Windows의 경우 내장 GGUF가 미지원되므로, 로컬 URL(Ollama, LM Studio 등) 또는 Gemini API 키 설정을 안내해야 합니다.
2. **에이전트 플랫폼**:
   - **Antigravity**: `agy` CLI 사용 가능 여부 확인.
   - **Claude Code**: Claude Code 세션 환경 확인.
   - **기타 MCP 클라이언트**: Cursor, Windsurf, Claude Desktop 등.

---

### 2단계: 설치 및 등록

#### 방법 A: Antigravity (`agy`)
터미널에서 아래 명령을 실행합니다:
```bash
# 로컬 저장소 기준 플러그인 설치:
agy plugin install ./plugins/memorize
```
플러그인 설치 확인:
```bash
agy plugin list
```

#### 방법 B: Claude Code
Claude 세션에서 플러그인 마켓플레이스 명령을 실행합니다:
```bash
/plugin marketplace add https://github.com/marbasdev/memorize.git
/plugin install memorize@memorize-marketplace
```

#### 방법 C: 단독 바이너리 / Windows / 범용 MCP
CGO 없는 순수 Go 정적 바이너리를 빌드합니다:
```bash
CGO_ENABLED=0 go build -trimpath -o bin/memorize ./cmd/memorize
```
클라이언트 MCP 설정 파일(`claude_desktop_config.json`, `.cursor/mcp.json` 등)에 등록합니다:
```json
{
  "mcpServers": {
    "memorize": {
      "command": "/절대경로/memorize/bin/memorize"
    }
  }
}
```
*(macOS/Linux의 경우 자동 갱신 래퍼인 `./plugins/memorize/scripts/run-memorize.sh` 등록 권장).*

---

### 3단계: 자가 진단 (`memorize doctor`)

자가 진단 명령을 실행합니다:
```bash
memorize doctor
# 또는
./bin/memorize doctor
```
기계 판독용 JSON 출력이 필요한 경우:
```bash
memorize doctor --json
```

**진단 점검 항목**:
1. `Binary & Plugin`: 실행 중인 바이너리 버전이 플러그인 매니페스트(`v1.2.0`)와 일치하는지 확인.
2. `Daemon Process`: 데몬 프로세스가 동작 중이거나 `http://127.0.0.1:18095`에서 자동 기동 준비 상태인지 확인.
3. `Storage`: SQLite WAL 모드 활성화 및 `busy_timeout`이 10000ms 이상인지 확인.
4. `Git & Scope`: 프로젝트 표준 루트 및 `project:<저장소>` 스코프 감지 확인.
5. `Inference Engine`: 활성 추론 프로바이더(Local GGUF, Gemini, 로컬 URL, JEV 등) 설정 여부 확인.
6. `Team Remote`: 원격 팀 서버 연결 여부 확인(솔로 모드 시 선택 사항).

경고(`[WARN]`)나 실패(`[FAIL]`) 항목이 있다면 `memorize doctor`가 출력하는 해결 안내를 따르세요.

---

### 4단계: 추론 엔진 확인 및 안내

추론 모델 설정 상태를 확인합니다:
- `memorize doctor`에서 `[WARN] Inference Engine`이 보고되는 경우:
  - 기억 커밋 및 검색 등 핵심 동작에 추론 엔진이 필수임을 개발자에게 알립니다.
  - 웹 콘솔(**`http://127.0.0.1:18095`**, 추론 엔진 탭)에서 **로컬 GGUF 모델을 1클릭 다운로드**(macOS/Linux)하거나 API 키를 입력하도록 안내합니다.
  - 또는 환경 변수를 통해 Gemini API 키를 직접 설정할 수도 있습니다:
    ```bash
    export GEMINI_API_KEY="your-api-key"
    ```

---

### 5단계: 프로젝트 온보딩 및 규칙 초기화

1. `team_rules` 또는 `query_project_memory`를 조회하여 현재 저장소의 규칙 상태를 파악합니다.
2. 현재 프로젝트의 활성 기억 수를 확인합니다:
   - 기억이 `0`개인 경우, 개발자에게 다음을 묻습니다:
     > *"이 저장소에 등록된 기억이 없습니다. 코드베이스의 핵심 파일을 분석하여 프로젝트 아키텍처 및 코딩 규칙을 memorize에 온보딩할까요?"*
3. 개발자가 수락하면:
   - 핵심 파일(`README.md`, `go.mod` / `package.json`, 아키텍처 문서, 설정 파일 등)을 검토합니다.
   - 레거시 패턴과 신규 패턴이 상충하는 경우, 규칙 저장 전에 **어느 기준이 우선인지 개발자에게 먼저 확인**합니다.
   - 확정된 규칙을 `commit_memory` 또는 `manage_memory(action="bootstrap_rules")`로 커밋합니다.
   - 완료 후 `project_progress(action="record")`로 진행 상황을 기록합니다.
4. 개발자가 건너뛰기를 원하면, `manage_memory(action="skip_onboarding")`를 호출하여 재안내를 건너뜁니다.
