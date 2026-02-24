package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

func main() {
	var wg sync.WaitGroup
	totalUsers := 50000

	for i := 0; i < totalUsers; i++ {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()

			userID := fmt.Sprintf("user_%d", user)
			url := fmt.Sprintf("http://localhost:8080/ticket?user_id=%s", userID)

			for {
				resp, err := http.Get(url)
				if err != nil {
					return
				}

				switch resp.StatusCode {
				case http.StatusOK: // 200: 드디어 예매 성공!
					fmt.Printf("사용자 %d: ★ 예매 성공! (ID: %s)\n", user, userID)
					resp.Body.Close()
					return

				case http.StatusAccepted: // 202: 대기열 진입 성공 (순번 대기 중)
					var result map[string]interface{}
					json.NewDecoder(resp.Body).Decode(&result)

					// 대기 번호를 출력하며 1초 대기 후 재시도(Polling)
					fmt.Printf("사용자 %d: [대기중] 순번: %v (ID: %s)\n", user, result["rank"], userID)
					resp.Body.Close()

					time.Sleep(1 * time.Second) // 1초 뒤에 "저 이제 차례인가요?" 물어봄
					continue

				case http.StatusGone: // 410: 매진
					fmt.Printf("사용자 %d: [품절] 시도 중단 (ID: %s)\n", user, userID)
					resp.Body.Close()
					return

				case http.StatusBadRequest: // 400: 이미 구매함
					fmt.Printf("사용자 %d: [거절] 이미 구매한 사용자 (ID: %s)\n", user, userID)
					resp.Body.Close()
					return

				default:
					resp.Body.Close()
					return
				}
			}
		}(i)
	}

	wg.Wait()
	fmt.Println("테스트 종료")
}
