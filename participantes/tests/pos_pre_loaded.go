package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Request/Response payloads based on README contract
// POST /api/find-service
// { "intent": "string" }
// Response: { success: bool, data: { service_id: int, service_name: string }, error: string }

type apiRequest struct {
	Intent string `json:"intent"`
}

type apiResponse struct {
	Success bool        `json:"success"`
	Data    apiRespData `json:"data"`
	Error   string      `json:"error"`
}

type apiRespData struct {
	ServiceID   int    `json:"service_id"`
	ServiceName string `json:"service_name"`
}

func execute_tests() {
	var (
		inCSV     string
		outCSV    string
		endpoint  string
		timeoutMs int
	)

	flag.StringVar(&inCSV, "in", "assets/intents_pos_loaded.csv", "Caminho do CSV de entrada (semicolon ';' separado) contendo as intenções")
	flag.StringVar(&outCSV, "out", "assets/found_services.csv", "Caminho do CSV de saída onde será salvo o service_name retornado")
	flag.StringVar(&endpoint, "url", "http://localhost:16081/api/find-service", "URL do endpoint /api/find-service")
	flag.IntVar(&timeoutMs, "timeout", 15000, "Timeout em milissegundos para a requisição HTTP")
	flag.Parse()

	// Abrir CSV de entrada
	inFile, err := os.Open(inCSV)
	if err != nil {
		fmt.Fprintf(os.Stderr, "erro ao abrir CSV de entrada: %v\n", err)
		os.Exit(1)
	}
	defer inFile.Close()

	reader := csv.NewReader(inFile)
	reader.Comma = ';'

	// Criar CSV de saída
	outFile, err := os.Create(outCSV)
	if err != nil {
		fmt.Fprintf(os.Stderr, "erro ao criar CSV de saída: %v\n", err)
		os.Exit(1)
	}
	defer outFile.Close()

	writer := csv.NewWriter(outFile)
	writer.Comma = ';'
	defer writer.Flush()

	// Cabeçalho do CSV de saída
	if err := writer.Write([]string{"service_id", "service_name", "intent", "success", "error"}); err != nil {
		fmt.Fprintf(os.Stderr, "erro ao escrever cabeçalho: %v\n", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: time.Duration(timeoutMs) * time.Millisecond}

	row := 0
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "erro ao ler CSV (linha %d): %v\n", row+1, err)
			os.Exit(1)
		}
		row++

		// Esperado: service_id;service_name;intent
		if len(rec) < 3 {
			// pular linhas inválidas
			continue
		}

		if rec[0] == "service_id" { // pular cabeçalho de entrada
			continue
		}

		intent := strings.TrimSpace(rec[2])
		payload := apiRequest{Intent: intent}
		body, _ := json.Marshal(payload)

		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			fmt.Fprintf(os.Stderr, "erro ao criar requisição: %v\n", err)
			os.Exit(1)
		}
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := client.Do(req)
		latency := time.Since(start)
		if err != nil {
			// escrever erro na saída para manter rastreabilidade
			_ = writer.Write([]string{"", "", intent, "false", fmt.Sprintf("http error: %v", err)})
			fmt.Fprintf(os.Stderr, "falha na requisição (%.0fms): %v | intent: %s\n", latency.Seconds()*1000, err, intent)
			continue
		}

		func() {
			defer resp.Body.Close()
			var apiResp apiResponse
			if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
				_ = writer.Write([]string{"", "", intent, "false", fmt.Sprintf("decode error: %v", err)})
				fmt.Fprintf(os.Stderr, "erro ao decodificar resposta: %v | intent: %s\n", err, intent)
				return
			}

			if !apiResp.Success {
				_ = writer.Write([]string{"", "", intent, "false", strings.TrimSpace(apiResp.Error)})
				fmt.Fprintf(os.Stderr, "resposta sem sucesso (%.0fms): %s | intent: %s\n", latency.Seconds()*1000, apiResp.Error, intent)
				return
			}

			// Sucesso: gravar service_id e service_name retornados, junto da intent
			_ = writer.Write([]string{
				fmt.Sprintf("%d", apiResp.Data.ServiceID),
				apiResp.Data.ServiceName,
				intent,
				"true",
				"",
			})
		}()
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		fmt.Fprintf(os.Stderr, "erro ao finalizar escrita do CSV: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Processo concluído. Saída salva em: %s\n", outCSV)
}
