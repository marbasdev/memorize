# Team Rules (`memorize`)

> Shared repository rules managed via Git. Mirrored read-only into local `memorize` daemons.

## Architecture (`architecture`)

- (`engine`) internal/domain 패키지는 다른 internal/* 패키지를 임포트하지 않는 순수 도메인 타입 및 정규화 함수 계층으로 유지합니다. · `@memorize-dev` · `2026-11-14`
- (`engine`) internal/modules/* 패키지는 internal/transport/*나 internal/app/*를 임포트하지 않으며, CLI·HTTP·MCP·대시보드 전달 계층이 modules와 inference를 호출하는 단방향 의존성을 유지합니다. · `@memorize-dev` · `2026-11-14`
- (`workflow`) 프로덕션 코드의 git 명령 호출은 internal/infra/git 내부에만 두고 modules나 transport에서 직접 실행하지 않습니다. · `@memorize-dev` · `2026-11-14`
- (`database`) SQLite DB 연결과 스키마 초기화는 internal/infra/sqlite의 Open과 InitSchema로 일원화하며, 팀 룰 본문은 DB 미러 없이 데몬 인메모리 캐시와 .memorize/TEAM_RULES.md에서 직접 읽는다. · `@memorize-dev` · `2026-11-14`
- (`dashboard`) 서드파티 번들을 제외한 internal/transport/dashboard/assets 및 templates 커스텀 파일은 각 800줄 미만 모듈로 분리 유지합니다. · `@memorize-dev` · `2026-11-14`
- (`workflow`) README.md·README.ko.md·ARCHITECTURE.md·ARCHITECTURE.ko.md·TEAM.md·TEAM.ko.md 등 공개 제품·아키텍처·가이드 문서는 개별 구현·리팩터링 작업마다 수시로 갱신하지 않고 버전 업(릴리스) 시점에 일괄 동기화한다(단, 사용자 직접 지시와 .memorize/TEAM_RULES.md·스킬/규칙 파일은 예외). · `@memorize-dev` · `2026-11-14`

## Security (`security`)

- (`release`) 엔진 탐색 및 어댑터에서 저장된 API 키는 u.Hostname() 정확 일치로 공식 호스트(api.typesafe.ai) 또는 기존 저장 호스트에만 전송한다. · `@memorize-dev` · `2026-11-14`
- (`release`) cwd 및 runtime.Caller 기반 바이너리 자동 교체와 라이브 에셋 로드는 기본 비활성화하고 MEMORIZE_DEV_HOTRELOAD=1일 때만 허용한다. · `@memorize-dev` · `2026-11-14`

## Testing (`testing`)

- (`go`) CGO_ENABLED=0 순수 Go 정적 빌드를 유지하며, 코드 변경 후 gofmt -l ., GOOS=darwin|linux|windows 대상 CGO_ENABLED=0 go vet ./..., go test -race -count=1 ./...를 실행해 검증한다. · `@memorize-dev` · `2026-11-14`

## Convention (`convention`)

- (`http`) internal/transport/http의 JSON POST 엔드포인트는 수동 헤더 검사 대신 security.go의 미들웨어와 유틸리티를 사용해 루프백 Host/Origin 및 Sec-Fetch-Site 검증을 일관되게 적용합니다. · `@memorize-dev` · `2026-11-14`
- (`workflow`) 훅·분류기·대시보드·진행 현황 로직에서 하드코딩된 단어·불용어 사전, 특정 응답 문구 목록, 문자열 역파싱 휴리스틱, 커스텀 DOM morpher를 추가하지 않는다. · `@memorize-dev` · `2026-11-14`
- (`dashboard`) internal/transport/dashboard/i18n.go에 UI 문구 키를 추가·수정할 때는 영문·한글 맵에 동일 키를 함께 동기화한다. · `@memorize-dev` · `2026-11-14`
