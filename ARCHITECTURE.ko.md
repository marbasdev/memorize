# 아키텍처 및 기술 레퍼런스

[English (ARCHITECTURE.md)](./ARCHITECTURE.md) | [README로 돌아가기](./README.ko.md)

## 1. 내부 동작 파이프라인

```mermaid
flowchart LR
    A["무상태 Stdio 브리지 / Streamable HTTP (/mcp) / 훅"] --> B["단일 로컬 데몬 (127.0.0.1:18095)"]
    B --> C["쓰기 게이트키퍼 (노이즈 폐기 / 1:N 덮어쓰기)"]
    B --> D["병렬 전수 평가 (관련도 채점 · 계획 검증)"]
    D --> E["세션 델타 & [REVOKED] 필터"]
    D --> F["PreToolUse / git pre-commit 가드레일"]
```

### 1.1 단일 로컬 데몬 (`127.0.0.1:18095`)
- **자동 기동 및 통합 소유**: 첫 `stdio` 또는 훅 호출 시 자동 기동되어 SQLite(`WAL`), `llama-server` GGUF 런타임(기본 `:18088`), SSE 웹 콘솔, `/mcp`, `/api/daemon/mcp`, `/api/daemon/hook`, `/api/daemon/port`를 통합 관리합니다.
- **포트 관리 및 자동 폴백**: 설정 포트는 DB와 같은 경로의 `ports.json`에 저장하고 실제 바인딩 주소는 `daemon.json`에 게시합니다. 포트 충돌 시 빈 루프백 포트로 자동 폴백하며 CLI·훅·열린 `stdio` 브리지가 새 주소를 즉시 추적합니다.
- **핫리로드 및 실행 규약**: 업그레이드된 버전 바이너리를 무중단 교체하되(`CWD` 바이너리 감시 및 디스크 템플릿·에셋 라이브 리로드는 `MEMORIZE_DEV_HOTRELOAD=1`일 때만 허용), 매니페스트 기반 동적 `timeout_ms`를 적용하고(`git pre-commit`은 무제한) 정적 `AGENTS.md`/`CLAUDE.md` 대신 MCP `serverInstructions`·훅·SQLite 규칙만으로 동작합니다.

### 1.2 쓰기 게이트키퍼 & 계보 관리
- **노이즈 분류·폐기**: 후보 기억을 `invariant` · `rule` · `preference` · `transient`로 분류해 일회성 노이즈를 버립니다.
- **원자적 `1:N` 덮어쓰기**: 배타적 충돌이 감지되면 단일 트랜잭션으로 기존 규칙을 `active=0, superseded_by`로 비활성화합니다.
- **세션 무효화 통지**: 후속 턴에 `*(Replaces: ...)*`, `*(Updated #id ...)*`, `[REVOKED #id]`를 주입해 옛 지시 회귀를 차단합니다.

### 1.3 세션 델타 · 전수 병렬 평가 · 컴팩션 자동 복구
- **세션 선필터 & KV 캐시**: 평가 전 `session_sent`로 이미 전달된 ID를 걸러 변경 없는 반복 턴은 추론 `0`회로 통과시키고, 미전달 규칙 100%를 `고정 Prefix + 동적 Suffix` 병렬 청크로 평가해 `TOP-K` 누락 없이 KV 캐시 히트와 최소 메인 컨텍스트(`System 2`)를 동시에 달성합니다.
- **컴팩션 자동 복구**: Claude Code의 `PostCompact`/`SessionStart(source="compact")` 및 Antigravity `transcriptPath`의 증분 `CHECKPOINT` 감지(`session_compacts`) 시 `session_sent`를 자동 초기화하고, 압축 요약을 관련도 평가에 반영해 필요한 규칙을 즉시 재주입합니다.

### 1.4 직관(`System 1`) 0-토큰 판정 · 태스크 라우팅 · 텔레메트리
- **직관(`System 1`) & 기능별 라우팅**: 게이트키핑·관련도·가드레일(`relevance`, `write_gate`, `verify`, `calibrate`)은 `Jev / Flide` 및 `gemini-decision-1`(`v1beta/decisions`, 출력 토큰 `0`) 단일 패스로 처리하고, 텍스트 합성(`consolidate`, `progress`, `taxonomy`)만 컴패니언 생성 모델(`gen_provider`/`gen_model`)로 분리 라우팅합니다. *(최초 설치 후 로컬 GGUF 다운로드 또는 API/URL 엔진 설정 완료 시 활성화)*
- **런타임 텔레메트리**: `eval_telemetry`에 7개 태스크·엔진별 호출 수, 평균/P95/누적 지연(ms), 타임아웃·오류를 집계해 추론 엔진 탭 상단 접이식 텔레메트리 패널에 표시합니다.

### 1.5 저장소 격리 (`project_progress` · 라이브 세션 · `guardrail_logs`)
- **최소주의 원칙**: 하드코딩 단어 사전, 문자열 스크래핑, 커스텀 DOM 모핑, 청소 타이머를 두지 않고 명시적·구조적 데이터만 유지합니다.
- **영구 진행판 (`project_progress`) & 인메모리 라이브 세션**: `memories`와 테이블을 분리해 규칙 오탐을 막고, 실시간 턴·툴 상태와 훅 누적 지연 시간은 90초 TTL 인메모리 `LiveSession` 레지스트리(`GET /api/live-sessions`, `event: live` SSE)로만 1줄 표시합니다.
- **가드레일 로그 & SSE**: `guardrail_logs.elapsed_ms`에 검사 시간을 기록하고, 영구 도메인은 `GET /api/events`(`event: change` SSE)로 부분 갱신하며 Auto-TTL(`0`=영구) 및 `PASS` 우선/워터마크 안전 정리를 지원합니다.

---

## 2. 패키지 아키텍처

### 2.1 폴더 구조

```text
memorize/
├── cmd/
│   └── memorize/                  # 최소 진입점 (main.go에서 RuntimeHooks 주입 후 transport/cli 위임)
│
├── internal/
│   ├── domain/                    # [1] 순수 도메인 모델 & 계약 (표준 라이브러리만 사용; DB/HTTP/Git 무의존)
│   │                              #     Fact, Scope, Progress, GuardrailLog, Evaluator, ChangeEvent, 비밀 마스킹
│   │
│   ├── infra/                     # [2] 기술 기반 인프라 (비즈니스 규칙 없음, 노드 공통 배관)
│   │   ├── config/                #     환경변수, data dir 경로, 원자적 ports.json / daemon.json 설정 저장소
│   │   ├── sqlite/                #     WAL SQLite 커넥션, 스키마 초기화, repo_sync 헬퍼
│   │   ├── git/                   #     Git 루트 탐지, HEAD / 커밋 diff 및 로그 조회
│   │   └── eventbus/              #     인프로세스 SSE 변경·라이브 세션 이벤트 브로드캐스터
│   │
│   ├── inference/                 # [3] AI 추론 엔진 계층 (도메인 규칙 비의존)
│   │   ├── router/                #     태스크별 라우팅, 429 fallback 쿨다운 체인, eval_telemetry 기록
│   │   ├── providers/             #     Gemini (decisions 포함), System 1 (Jev/Flide), OpenAI 호환, GGUF 어댑터
│   │   ├── discovery/             #     추천 모델 프리셋 카탈로그 및 로컬 엔드포인트 프로빙
│   │   └── localmodel/            #     llama-server 수명주기 및 GGUF 다운로드 관리 (macOS/Linux 로컬 전용)
│   │
│   ├── modules/                   # [4] 비즈니스 수직 모듈 (자기 테이블·규칙만 소유 → 엄격한 SSOT 격리)
│   │   ├── memory/                #     기억 CRUD, 1:N 덮어쓰기, 세션 델타, 병합 정리, 분류, 플레이북
│   │   ├── progress/              #     project_progress 보드, Git 커밋 동기화, 라이브 세션, TTL 리텐션
│   │   └── guardrail/             #     IDE/Git 훅 디스패치, edit diff 검사, guardrail_logs, 온보딩
│   │
│   ├── transport/                 # [5] 외부 프로토콜 어댑터 (입출력 변환만 담당)
│   │   ├── cli/                   #     플래그 파싱, stdio 브리지, daemon/port CLI, install (RuntimeHooks 주입)
│   │   ├── mcp/                   #     MCP 툴·리소스·프롬프트 등록 및 워크스페이스 스코프 해석
│   │   ├── http/                  #     공용 HTTP 라우터, /mcp·데몬 브리지 경로, 루프백/CSRF 보안 가드
│   │   └── dashboard/             #     HTMX SSR 웹 콘솔 (templates/, assets/, i18n, 탭 핸들러)
│   │
│   └── app/                       # [6] 실행 모드 조립 (Wiring / Lifecycle)
│       └── localdaemon/           #     단일 로컬 데몬 기동, 포트 폴백 및 버전 무중단 바이너리 핸드오버
│
└── plugins/                       # 에이전트 플러그인 매니페스트, 훅 러너 및 스킬 (Claude Code, Antigravity)
```

### 2.2 의존성 및 격리 규칙
- **단방향 비순환 계층**: `domain -> infra -> inference -> modules -> transport -> app <- cmd/memorize`. 모든 패키지는 하위 계층만 임포트하며 상위 계층을 참조하지 않습니다 (`domain`은 Go 표준 라이브러리만 사용).
- **계층 내부 의존 방향**:
  - `infra`: `sqlite`가 `config`·`git`을 임포트하며, `eventbus`·`config`·`git`은 서로 독립적입니다.
  - `inference`: `router`가 `localmodel`·`providers`를 임포트하고, `discovery`는 `providers`를 임포트합니다.
  - `modules`: `guardrail`이 `progress`·`memory`를 임포트하고, `progress`가 `memory`를 임포트합니다 (타 모듈 테이블 직접 침범 없이 서비스 계약을 통해서만 연계).
  - `transport`: `cli`와 `dashboard`가 `http`를 임포트하고, `http`가 `mcp`를 마운트해 루프백/CSRF 보안 미들웨어·브리지 타입을 공용화합니다.
- **진입점 역의존 차단 (`RuntimeHooks`)**: `cmd/memorize/main.go`가 `internal/app/localdaemon`에서 생성한 `RuntimeHooks`를 `internal/transport/cli`에 주입하므로, `transport/*` 계층은 `app/*`를 직접 임포트하지 않습니다.

### 2.3 로컬 전용 vs 동기화 경계 & `v1.1+` 확장 포인트
로컬 노드 전용 배관과 이식 가능한 도메인 상태를 분리해 두었기 때문에, `v1.0` 이후 팀 공용 서버 및 기기 간 동기화 기능을 추가할 때 기존 비즈니스 규칙을 재작성하지 않고 확장할 수 있습니다.

| 경계 구분 | 포함 패키지 | 담당 영역 |
| :--- | :--- | :--- |
| **로컬 노드 전용** | `infra/config`, `inference/localmodel`, `modules/guardrail`, `app/localdaemon` | API 키, 루프백 포트·PID 상태, 로컬 `llama-server`/GGUF, 머신 훅 및 라이브 세션 |
| **동기화 가능 상태** | `modules/memory`, `modules/progress` | 스코프 기반 기억 계보(`global`, `project:*`, `personal`) 및 명시적·Git 진행 상태 |

- **예정된 `v1.1+` 확장 위치** *(현재 디렉터리 트리에는 미생성)*:
  - `internal/modules/sync`: `memory`/`progress` 변경분을 기기·팀 간 delta push/pull로 동기화
  - `internal/app/teamserver`: `inference/localmodel` 없이 인증·테넌트 가드와 `modules/sync`를 조합해 기동하는 팀 공용 서버 진입 조립기

---

## 3. MCP 도구, 리소스 및 프롬프트 명세

| 종류 | 이름 | 설명 |
| :--- | :--- | :--- |
| **Tool** | `query_project_memory` | 대상 스코프(`project:<repo>`, `global`, `personal`) 활성 기억 전수 평가 + 세션 델타 |
| **Tool** | `commit_memory` | 단건/다건 후보 평가(저장·`1:N` 덮어쓰기·폐기) + Git `synced_commit` 갱신 |
| **Tool** | `verify_plan_against_memory` | 코드 변경 전 작업 계획의 활성 규칙 위반 여부 검사 |
| **Tool** | `manage_memory` | `update`, `forget`, `undo`, `consolidate`, `purge`, `skip_onboarding`, `reset_onboarding` |
| **Tool** | `project_progress` | 분리된 3칸 진행 보드(`planned` / `in_progress` / `blocked` / `done`)·기억/Git 동기화: `record`, `update`, `list`, `sync`, `delete` |
| **Resource** | `memorize://playbook/memory-curator` | 바이너리 내장 운영 플레이북 |
| **Prompt** | `memory-curator` | 고순도 상태 규칙 정제용 프롬프트 템플릿 |

---

## 4. 환경변수 레퍼런스

| 변수명 | 기본값 | 설명 |
| :--- | :--- | :--- |
| `MEMORY_DATA_DIR` | `~/.memorize` | `memories.db`, `bin/`, `models/`, `ports.json`, `daemon.json` 공통 기본 루트 (Windows: `%USERPROFILE%\.memorize`) |
| `MEMORY_DB_PATH` | `~/.memorize/memories.db` | SQLite WAL DB 경로 (`0600` 권한) |
| `MEMORY_MODELS_DIR` | `~/.memorize/models` | 로컬 GGUF 및 `llama-server` 저장 경로 (`macOS`/`Linux`) |
| `MEMORY_PROVIDER` | `gguf` *(macOS/Linux)* / `local` *(Windows)* | `gguf`, `local`, `jev` / `flide`, `gemini` |
| `GEMINI_API_KEY` / `GEMINI_MODEL` | *(비어 있음)* / `gemini-3.5-flash-lite` | Gemini 키·모델 (`gemini-decision-1`은 `/v1beta/decisions` 라우팅) |
| `JEV_API_URL` / `JEV_MODEL` | `http://127.0.0.1:8000` / `jev-latest` | System 1 API (`OmniJev/Flide` 또는 TypeSafe Cloud; `TYPESAFE_API_KEY` 선택) |
| `LOCAL_MODEL_URL` / `LOCAL_MODEL_NAME` | `http://127.0.0.1:11434/v1` / *(자동)* | OpenAI 호환 서버 (`Ollama`, `LM Studio`, `vLLM`; `LOCAL_API_KEY` 선택) |
| `MEMORY_DAEMON_ADDR` | *(미설정 시 `ports.json` / 기본 `127.0.0.1:18095`)* | 데몬 및 웹 콘솔 주소 수동 고정 (`memorize daemon port`보다 우선) |
| `MEMORY_HOOK_TIMEOUT` | *(매니페스트 기준)* | 훅 제한 시간 오버라이드 (`8s`, `0`/`off` 무제한) |
| `MEMORY_INCLUDE_PERSONAL` | `0` | `1` 설정 시 프로젝트 조회에 `personal` 기억 포함 |
| `MEMORY_DISABLE_BG_DASHBOARD` | `0` | `1` 설정 시 독립 인라인 MCP `stdio` 모드 |
| `MEMORY_GUARDRAIL_FAIL_CLOSED` | `0` | `1` 설정 시 PreToolUse·가드레일 검증 실패/엔진 미설정 상태에서 도구 실행을 차단 |
| `MEMORIZE_DEV_HOTRELOAD` | `0` | 개발용 옵트인 (`1` 설정 시 `CWD` 바이너리 감시 및 디스크 템플릿/에셋 라이브 리로드 활성화) |
| `MEMORIZE_DEV_BUILD` | `0` | `run-memorize.sh`에서 `1` 설정 시 릴리스 SHA256 다운로드 대신 로컬 Go 소스 빌드 폴백 허용 |
| `MEMORIZE_RELEASE_BASE_URL` | *(`plugin.json` 기준)* | 커스텀 미러 URL (`GITHUB_TOKEN` / `MEMORIZE_GITLAB_TOKEN` 지원) |

---

## 5. CI/CD 자동 릴리스

- **파이프라인 정의**: [GitHub Actions CI (`ci.yml`)](./.github/workflows/ci.yml) · [GitHub Actions Release (`release.yml`)](./.github/workflows/release.yml) · [GitLab CI (`.gitlab-ci.yml`)](./.gitlab-ci.yml)
- **자동 테스트 검증**: 멀티 OS `go vet` + Unix(`ubuntu-latest`, `macos-latest`) `go test -v -race -count=1 ./...` + Windows(`windows-latest`) `CGO_ENABLED=0 go test -v -count=1 ./...`
- **릴리스 아티팩트 (`v*.*.*` 태그)**: 5개 타겟(`darwin/arm64`, `darwin/amd64`, `linux/arm64`, `linux/amd64`, `windows/amd64`) `CGO_ENABLED=0` 아카이브 및 `SHA256SUMS` 자동 생성·배포
