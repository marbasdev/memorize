# 팀 협업 & 다기기 가이드

[English (TEAM.md)](./TEAM.md) · [README로 돌아가기](./README.ko.md) · [아키텍처 레퍼런스](./ARCHITECTURE.ko.md)

목적에 따라 두 가지 방식을 단독 또는 조합해 사용할 수 있습니다:

| 방식 | 추천 상황 | 인프라 요구 사항 |
| :--- | :--- | :--- |
| **1. Git 팀 규칙 (`.praxis/TEAM_RULES.md`)** | 별도 서버 운영 없이 저장소 코드와 함께 팀 핵심 규칙만 동기화할 때 | 추가 서버 불필요 (각자 로컬 데몬 사용) |
| **2. 공유 팀 서버 (`praxis serve` + `connect`)** | 팀 전체가 중앙 추론 엔진·프로젝트 기억을 실시간 공유하거나 **1인이 여러 PC에서 같은 기억을 이어 쓸 때** | 서버 1대 실행 · 클라이언트 PC는 로컬 모델/DB 불필요 |

---

## 1. Git 기반 팀 규칙 (`.praxis/TEAM_RULES.md`)

채팅 턴·훅 실행 시 `.praxis/TEAM_RULES.md` 변경을 자동 감지해 규칙 주입과 가드레일(`PreToolUse` · `Stop` · `git pre-commit`) 최우선 순위로 반영합니다.

> [!IMPORTANT]
> **`.praxis/TEAM_RULES.md` 파일을 직접 손으로 편집하지 마세요.**
> 반드시 **웹 콘솔(`팀 규칙` 탭 / `팀 규칙으로 승격...`)** 또는 **MCP `team_rules`** 도구로만 변경해야 임시 `git worktree`에서 해당 파일만 단독 커밋·푸시되어 작업 중인 브랜치 코드와 섞이지 않습니다.

- **웹 콘솔**: `기억` 탭에서 로컬 항목을 1줄 팀 규칙으로 **`승격(Promote)`**하거나, **`팀 규칙`** 탭에서 추가·수정·`원격 동기화`·중복 정리·충돌 해결을 수행합니다.
- **MCP 도구 (`team_rules`)**: `list` · `sync` · `add` · `promote` · `update` · `remove` · `resolve_conflict` · `duplicates` · `clean_duplicates` · `set_config`

---

## 2. 공유 팀 서버 & 다기기 연결 (`praxis serve` · `connect`)

단일 바이너리와 SQLite(`WAL`)만으로 인증된 서버를 띄우고, 팀원(또는 본인의 서브 PC)은 가벼운 씬 클라이언트로 연결합니다.

### 서버 기동 및 계정 발급
```bash
praxis serve --addr 0.0.0.0:18095        # 최초 실행 시 초기 admin 토큰 1회 출력
praxis user add alice --role member      # 팀원 발급 (list | rotate | disable 지원, 웹 콘솔 'Accounts' 모달 동일)
```
> **외부 망 노출 시**: 사설망 또는 리버스 프록시(Caddy · Nginx · Traefik) 뒤에서 HTTPS(TLS)를 적용해 Bearer 토큰과 세션 쿠키를 보호하세요.

### 클라이언트 연결 및 계정 전환
```bash
# 팀 서버 연결 (웹 콘솔 상단 'Remote Servers' 모달에서도 설정 가능)
praxis connect --global --url https://praxis.company.com --token prx_...
praxis connect --url https://praxis.company.com --token prx_... --repo .   # 현재 저장소만 연결
praxis disconnect --global                                                 # 연결 해제 (또는 --repo .)

# 다중 계정 즉시 전환 (개인 ↔ 업무 계정 등, IDE 재시작 불필요)
praxis switch                            # 저장된 프로필 목록 확인
praxis switch alice                      # 전역 전환 (현재 저장소만 전환 시 --repo .)
```
- **클라이언트 파일 비접근**: Git diff와 `.praxis/TEAM_RULES.md` 스냅샷은 클라이언트 측에서 읽어 전송하며 서버는 호스트 파일시스템을 탐색하지 않습니다.
- **일반 MCP 직접 연결**: 훅 없는 클라이언트는 `https://<host>/mcp`에 `Authorization: Bearer <token>` 헤더를 지정해 바로 연결할 수 있습니다.
- **웹 콘솔 계정 스위처**: 상단 **`@account · role ▾`** 배지에서 평문 토큰 노출 없이(`HttpOnly` 서명 쿠키) 최대 10개 계정을 1클릭 전환합니다.

### 로컬 → 서버 이관 & 포터블 번들 (`push` · `export` · `import`)
```bash
praxis push --scope global --dry-run                       # 업로드 전 미리보기
praxis push --scope project:app --mode merge               # exact(기본 1:1) 또는 merge(AI 중복 병합)
praxis export --scope global -o global-rules.json          # 포터블 JSON 번들 내보내기
praxis import global-rules.json --mode merge --dry-run     # 번들 병합 미리보기 및 가져오기
```
*(웹 콘솔 `기억` 탭의 **`JSON 내보내기`** · **`가져오기 · 병합...`** 모달에서도 동일하게 지원합니다.)*

### 4계층 우선순위 및 계정별 프라이버시 격리

팀 표준과 프로젝트 맥락은 공유하되 개인 규칙은 계정별로 철저히 분리되며, 동일 순서로 정렬해 서버 KV 캐시를 공유합니다:

| 우선순위 | 스코프 | 공유 및 수정 범위 |
| :--- | :--- | :--- |
| **1 (최우선)** | **팀 규칙** (`.praxis/TEAM_RULES.md`) | 팀 전체 공통 표준 (서버 모드에서는 개인 예외 비활성화) |
| **2** | **`project:<repo>`** | 팀 전체 공유 프로젝트 규칙 · 활동 이력 (`origin` 원격명 기준 자동 매칭) |
| **3** | **`global`** | **계정별 격리** — 각자의 전 프로젝트 공통 개발 규칙 |
| **4** | **`personal`** | **계정별 격리** — 개발 외 개인 메모 (선택 시에만 프로젝트 조회에 포함) |
