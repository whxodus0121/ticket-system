# 🎫 티켓 예매 동시성 제어 프로젝트 (Ticket-System)

이 프로젝트는 동시성 요청이 발생하는 티켓 예매 상황에서 데이터의 정합성을 보장하고, 
시스템 부하를 최소화하는 과정을 단계별로 해결해 나가는 백엔드 시스템입니다.

## 📌 버전별 개발 기록 (Tags)

### 🔴 v1.0: 인프라 구축 및 기본 연동
- **주요 내용**: Redis 분산 락 및 MySQL 연동 완료
- **학습 포인트**: Docker를 이용한 Redis, MySQL 환경 구축 및 Go 언어와의 기본적인 커넥션 설정

### 🟡 v2.0: 동시성 이슈 해결 (초과 판매 방지)
- **주요 내용**: 초과 판매 방지, DB 연결 폭주 에러 해결, 예매 성공 명단 관리
- **해결 방법**: 
  - Redis 분산 락을 통한 재고 차감의 원자성 보장
  - DB 커넥션 풀 최적화로 `Too many connections` 에러 해결
- **결과**: 동시에 1,000명이 접속해도 설정된 재고만큼만 정확히 판매됨을 확인

### 🟢 v3.0: 비즈니스 정책 적용 (1인 1매 제한)
- **주요 내용**: 1인 1매 중복 방지 로직 추가
- **해결 방법**: 
  - **Double-Checked Locking**: 락 획득 전후로 구매 이력을 확인하여 중복 예매를 원천 차단
  - 고부하 상황에서 발생하는 **Slow SQL** 로그 분석을 통해 성능 병목 지점 파악
- **결과**: 동일 아이디로 무한 요청을 보내도 오직 1건의 구매 데이터만 남도록 보장

### 🔵 v4.0: 티켓 취소 로직 구현 및 시스템 성능 한계 도출
- **주요 내용**: 예매 취소 기능 구현 및 Redis-DB 간 데이터 정합성 보장
- **해결 방법**: 
  - 취소 프로세스 설계: MySQL 구매 내역 삭제 → Redis 재고 복구(+1) → Redis 구매자 명단 제거의 순차적 흐름 구현
  - 데이터 정합성 검증: DB에 실제 구매 내역이 있는 경우에만 취소를 승인하여, 비정상적인 재고 증가(Over-stocking)를 원천 차단
  - ⚠️ 직면한 문제 (Slow SQL 재발)
  - 현상: 동시 예매 테스트 시 MySQL INSERT 과정에서 0.5s 이상의 지연이 발생하는 Slow SQL 재발 확인
  - 원인 분석: Redis의 인메모리 처리 속도와 MySQL의 디스크 I/O 처리 속도 차이로 인한 동기식 저장의 병목 현상 발생
- **결과**: 예매와 취소의 전체 사이클은 완성되었으나, 고부하 환경에서의 안정적인 쓰기 성능을 위해 비동기 메시지 큐(Kafka) 도입의 필요성을 기술적으로 도출

### 🟣 v4.5: Kafka 비동기 처리 완성 및 분산 환경 멱등성 보장
- **주요 내용**: Kafka 도입을 통한 쓰기 지연(Write-Behind) 패턴 구현 및 메시지 중복 처리 최적화
- **해결 방법**: 
  - 비동기 저장 파이프라인: API 서버는 Kafka Producer로서 메시지를 발행하고 즉시 응답하며, 별도의 Consumer 워커가 MySQL 저장을 전담하여 Slow SQL 병목 해결
  - 멱등성(Idempotency) 설계: Kafka의 재전송 정책으로 인한 중복 데이터 유입 시에도 DB 무결성을 유지하도록 UNIQUE KEY 기반의 방어막 구축
  - DB 최적화 (GORM Clauses): OnConflict(DoNothing) 전략을 사용하여 중복 데이터 발생 시 불필요한 에러 로그 처리
  - 인터페이스 고도화: 리포지토리 함수 리턴 타입을 (bool, error)로 개선하여 실제 저장 성공 여부를 워커가 명확히 인지하도록 구현
- **결과**: 고부하 상황에서도 Redis의 처리 속도와 DB 저장 속도 간의 간극을 Kafka 버퍼로 해소하고, 데이터 정합성 100% 달성

### 🟤 v5.0: 비동기 정합성 완성 및 Lua 스크립트 기반 재고 보호
- **주요 내용**: Prometheus/Grafana 모니터링 시스템 도입 및 대규모 부하 테스트(50,000 TPS) 수행
  - 시스템 가시성(Visibility) 확보: 서버 및 워커에 Prometheus 메트릭 엔드포인트를 노출하여 요청 수, DB 저장 성공 수, 처리 지연 시간 등을 실시간 대시보드로 시각화
  - 분산 워커 시뮬레이션: 다중 워커 구조에서 Kafka 파티션별 메시지 분배 및 병렬 처리 효율성 검증
  - 대규모 트래픽 스트레스 테스트: go routine을 활용한 50,000명의 동시 접속 상황을 재현하여 시스템의 한계 지점 측정
- **결과**
  - 비동기 처리 이점 실증: 하단(요청) 그래프의 폭증에도 상단(DB 저장) 그래프가 완만한 경사를 유지하는 것을 확인하며, Kafka의 부하 평탄화(Load Leveling) 효과를 수치로 증명

    ![부하 테스트 결과](./images/grafana_result_v6.jpg)
    
  - 데이터 유실 제로: 5만 건의 몰입 트래픽 속에서도 최종 재고 수량과 DB 저장 수량이 100% 일치하는 무결성 확인
  - 인프라 안정성: 초당 수만 건의 요청 상황에서 Redis와 Kafka를 거치는 전체 파이프라인의 안정적인 가동 시간(Uptime) 확보

### ⚪ v6.0: Observability 구축 및 시스템 신뢰성 검증
- **주요 내용**: 비동기 취소 파이프라인 구축 및 재고 처리 로직 완성
  - Lua 스크립트를 이용한 원자적 재고 관리: 기존 DECR 방식의 한계를 극복하기 위해 Redis 내부에서 잔여 재고 확인 + 차감을 하나의 원자적 단위로 처리하는 Lua Script 도입
  - 비동기 취소 프로세스 (Delete-Behind): 취소 요청 시 Redis 재고를 즉시 복구하고 명단에서 제거하여 유저에게 **가용성(Availability)**을 즉시 반환
                                        실제 DB 데이터 삭제는 Kafka를 통해 비동기로 처리하여 예매와 동일한 고성능 쓰기 아키텍처 완성
- **결과**: 재고 정확도 확보: 무한 루프 예매/취소 테스트 시에도 재고가 항상 0 ~ MAX사이를 유지

---
## 🛠 Tech Stack
- **Language**: Go (Golang)
- **Database**: MySQL 8.0 (GORM)
- **Cache & Lock**: Redis
- **Message Broker**: Apache Kafka & Zookeeper
- **Container**: Docker, WSL2
- **Monitoring**: Prometheus, Grafana
- **Test**: Stress Testing with Goroutines

## 🚦 실행 방법 (How to Run)
1. **인프라 컨테이너 실행**
   Docker를 통해 필수 미들웨어(MySQL, Redis, Kafka, Zookeeper)를 한꺼번에 실행합니다.
   ```bash
   docker start ticket-mysql ticket-redis ticket-kafka ticket-zookeeper
   
2. **MySQL 테이블 생성 및 제약 조건 설정**

   중복 예매 방지(멱등성)를 위해 UNIQUE KEY가 포함된 테이블을 생성합니다.
   ```bash
   CREATE TABLE purchases (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id VARCHAR(255) NOT NULL,
    ticket_name VARCHAR(255) NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uk_user_ticket (user_id, ticket_name)
   );
   
4. **Kafka Consumer 워커 실행**
   Kafka 이벤트를 감시하며 DB에 저장하는 워커를 실행합니다. (다중 터미널 실행 권장)
   ```bash
   go run cmd/worker/main.go
5. **API 서버 및 동시성 테스트 실행**
   실제 예매 요청을 생성하여 시스템을 테스트합니다.
   ```bash
   go run main.go
