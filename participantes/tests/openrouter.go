package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

/*
Rodar:
  export OPENROUTER_API_KEY="sk-or-..."
  # (opcional)
  export OPENROUTER_MODEL="openai/gpt-4o-mini"

  go run main.go \
    -pre intents_pre_loaded.csv \
    -pos intents_pos_loaded.csv

Saída: relatório por arquivo (pre e pos), com latências e acurácia.
*/

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

func getModel() string {
	if m := os.Getenv("OPENROUTER_MODEL"); m != "" {
		return m
	}
	return "openai/gpt-4o-mini"
}

// ===== API types =====
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}
type choice struct {
	FinishReason       string      `json:"finish_reason"`
	NativeFinishReason string      `json:"native_finish_reason"`
	Message            chatMessage `json:"message"`
}
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
type chatResponse struct {
	ID      string   `json:"id"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
	Model   string   `json:"model"`
}

// ===== Domain =====
type service struct {
	ID   int
	Name string
}

// IMPORTANTE: apenas os 16 serviços válidos
var services = []service{
	{1, "Consulta Limite / Vencimento do cartão / Melhor dia de compra"},
	{2, "Segunda via de boleto de acordo"},
	{3, "Segunda via de Fatura"},
	{4, "Status de Entrega do Cartão"},
	{5, "Status de cartão"},
	{6, "Solicitação de aumento de limite"},
	{7, "Cancelamento de cartão"},
	{8, "Telefones de seguradoras"},
	{9, "Desbloqueio de Cartão"},
	{10, "Esqueceu senha / Troca de senha"},
	{11, "Perda e roubo"},
	{12, "Consulta do Saldo Conta do Mais"},
	{13, "Pagamento de contas"},
	{14, "Reclamações"},
	{15, "Atendimento humano"},
	{16, "Token de proposta"},
}

var serviceByID = func() map[int]string {
	m := make(map[int]string, len(services))
	for _, s := range services {
		m[s.ID] = s.Name
	}
	return m
}()

// CSV record
type csvRow struct {
	ServiceID   int
	ServiceName string
	Intent      string
}

// Resultado por linha
type caseResult struct {
	Idx          int
	Intent       string
	ExpectedID   int
	GotID1       int
	GotID2       int
	Dur1         time.Duration
	Dur2         time.Duration
	Err1         error
	Err2         error
	RawOut1      string
	RawOut2      string
	Model        string
	FinishReason string
}

// ===== Prompt =====
func buildSystemPrompt() string {
	var b strings.Builder
	b.WriteString("Você é um classificador determinístico de intenções.\n")
	b.WriteString("Tarefa: Receba um texto do usuário e escolha O ÚNICO serviço que melhor corresponde, retornando APENAS o número do ID do serviço (um número inteiro) e nada mais.\n")
	b.WriteString("NUNCA invente serviços, NUNCA invente IDs e NUNCA escreva nomes. Apenas o número do ID.\n")
	b.WriteString("Lista fixa de serviços válidos (ID: Nome):\n")
	for _, s := range services {
		fmt.Fprintf(&b, "%d: %s\n", s.ID, s.Name)
	}
	b.WriteString("\nRestrições:\n- Saída deve ser SÓ o número do ID (ex.: '4').\n- Temperature = 0.\n")
	return b.String()
}

func buildUserPrompt(intent string) string {
	return fmt.Sprintf("Entrada do usuário: %q\nRetorne apenas o ID (um inteiro).", intent)
}

// ===== HTTP =====
var httpClient = &http.Client{Timeout: 45 * time.Second}

func callOpenRouter(apiKey, model, sysPrompt, userPrompt string) (string, string, error) {
	bodyReq := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: sysPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0,
		MaxTokens:   8,
	}
	payload, err := json.Marshal(bodyReq)
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, openRouterURL, bytes.NewReader(payload))
	if err != nil {
		return "", "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HTTP-Referer", "https://example.com")
	req.Header.Set("X-Title", "Intent Matching Benchmark")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("status %d: %s", resp.StatusCode, string(respBytes))
	}

	var cr chatResponse
	if err := json.Unmarshal(respBytes, &cr); err != nil {
		return "", "", fmt.Errorf("unmarshal: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", "", errors.New("no choices")
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), cr.Model, nil
}

// ===== Parse ID =====
var onlyDigits = regexp.MustCompile(`^\D*?(\d{1,3})\D*$`)

func parseID(out string) (int, error) {
	m := onlyDigits.FindStringSubmatch(strings.TrimSpace(out))
	if len(m) < 2 {
		return 0, fmt.Errorf("resposta não contém um ID inteiro: %q", out)
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("ID inválido: %w", err)
	}
	if id < 1 || id > 16 {
		return 0, fmt.Errorf("ID fora da lista: %d", id)
	}
	return id, nil
}

// ===== CSV loader =====
func loadCSV(path string) ([]csvRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("abrir csv: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.Comma = ';'
	r.FieldsPerRecord = -1 // flexível
	r.ReuseRecord = true

	var rows []csvRow
	line := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("csv linha %d: %w", line, err)
		}
		if len(rec) < 3 {
			// pula cabeçalho vazio etc.
			continue
		}
		// pular cabeçalho se tiver
		if line == 1 && (strings.EqualFold(rec[0], "service_id") || strings.EqualFold(rec[2], "intent")) {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, fmt.Errorf("csv linha %d: service_id inválido: %v", line, err)
		}
		name := strings.TrimSpace(rec[1])
		intent := strings.TrimSpace(rec[2])
		// valida ID dentro do conjunto 1..16
		if _, ok := serviceByID[id]; !ok {
			return nil, fmt.Errorf("csv linha %d: service_id %d não existe na lista fixa (1..16)", line, id)
		}
		// (opcional) valida nome bater com a lista
		expName := serviceByID[id]
		if name != "" && !strings.EqualFold(name, expName) {
			log.Printf("[aviso] linha %d: nome no CSV difere do catálogo: CSV=%q, Catálogo=%q (usando catálogo)", line, name, expName)
		}
		rows = append(rows, csvRow{
			ServiceID:   id,
			ServiceName: expName,
			Intent:      intent,
		})
	}
	return rows, nil
}

// ===== Runner =====
type batchStats struct {
	Total         int
	Ok1           int
	Ok2           int
	SumDur1       time.Duration
	SumDur2       time.Duration
	ByServiceSeen map[int]int // quantos casos daquele serviço
	ByServiceOk1  map[int]int
	ByServiceOk2  map[int]int
}

func newBatchStats() *batchStats {
	return &batchStats{
		ByServiceSeen: make(map[int]int),
		ByServiceOk1:  make(map[int]int),
		ByServiceOk2:  make(map[int]int),
	}
}

func runBatch(label string, rows []csvRow, apiKey, model string) {
	sys := buildSystemPrompt()
	fmt.Printf("\n===== Rodada: %s =====\n", label)
	fmt.Printf("Casos: %d | Modelo: %s\n", len(rows), model)
	fmt.Println("----------------------------------------------------------------------------------------------")
	fmt.Printf("%-5s | %-5s | %-6s | %-6s | %-12s | %-12s | %s\n", "Idx", "Esper", "Got#1", "Got#2", "Lat#1(ms)", "Lat#2(ms)", "Intent")
	fmt.Println("----------------------------------------------------------------------------------------------")

	stats := newBatchStats()
	stats.Total = len(rows)

	for i, row := range rows {
		stats.ByServiceSeen[row.ServiceID]++

		user := buildUserPrompt(row.Intent)

		// call #1
		t1 := time.Now()
		out1, usedModel1, err1 := callOpenRouter(apiKey, model, sys, user)
		d1 := time.Since(t1)
		got1 := 0
		if err1 == nil {
			if id, e := parseID(out1); e == nil {
				got1 = id
				if got1 == row.ServiceID {
					stats.Ok1++
					stats.ByServiceOk1[row.ServiceID]++
				}
			} else {
				err1 = fmt.Errorf("parse: %w (raw=%q)", e, out1)
			}
		}
		_ = usedModel1 // mantido para futuro

		// call #2
		t2 := time.Now()
		out2, usedModel2, err2 := callOpenRouter(apiKey, model, sys, user)
		d2 := time.Since(t2)
		got2 := 0
		if err2 == nil {
			if id, e := parseID(out2); e == nil {
				got2 = id
				if got2 == row.ServiceID {
					stats.Ok2++
					stats.ByServiceOk2[row.ServiceID]++
				}
			} else {
				err2 = fmt.Errorf("parse: %w (raw=%q)", e, out2)
			}
		}
		_ = usedModel2

		stats.SumDur1 += d1
		stats.SumDur2 += d2

		fmt.Printf("%-5d | %-5d | %-6d | %-6d | %-12.2f | %-12.2f | %s\n",
			i+1, row.ServiceID, got1, got2, float64(d1.Milliseconds()), float64(d2.Milliseconds()), row.Intent)

		if err1 != nil || err2 != nil {
			if err1 != nil {
				fmt.Printf("    erro#1: %s\n", trimErr(err1.Error()))
			}
			if err2 != nil {
				fmt.Printf("    erro#2: %s\n", trimErr(err2.Error()))
			}
		}
	}

	avg1 := float64(stats.SumDur1.Milliseconds()) / float64(max(1, stats.Total))
	avg2 := float64(stats.SumDur2.Milliseconds()) / float64(max(1, stats.Total))
	acc1 := percent(stats.Ok1, stats.Total)
	acc2 := percent(stats.Ok2, stats.Total)

	fmt.Println("----------------------------------------------------------------------------------------------")
	fmt.Printf("Acurácia #1: %.1f%%  | Acurácia #2: %.1f%%\n", acc1, acc2)
	fmt.Printf("Latência média #1: %.2f ms | Latência média #2: %.2f ms\n", avg1, avg2)

	// per-service summary
	fmt.Println("\nResumo por serviço (ID: vistos / ok#1 / ok#2 / nome):")
	ids := make([]int, 0, len(services))
	for _, s := range services {
		ids = append(ids, s.ID)
	}
	for _, id := range ids {
		seen := stats.ByServiceSeen[id]
		if seen == 0 {
			continue
		}
		ok1 := stats.ByServiceOk1[id]
		ok2 := stats.ByServiceOk2[id]
		fmt.Printf("  %2d: %3d / %3d / %3d  - %s\n", id, seen, ok1, ok2, serviceByID[id])
	}
}

func percent(x, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(x) / float64(total) * 100.0
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func trimErr(s string) string {
	const max = 180
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ===== main =====
func main() {
	log.SetFlags(0)

	var prePath, posPath string
	flag.StringVar(&prePath, "pre", "intents_pre_loaded.csv", "caminho do CSV de pré-carregados")
	flag.StringVar(&posPath, "pos", "intents_pos_loaded.csv", "caminho do CSV de pós-carregados")
	flag.Parse()

	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		log.Fatal("Defina OPENROUTER_API_KEY no ambiente")
	}
	model := getModel()

	preRows, err := loadCSV(prePath)
	if err != nil {
		log.Fatalf("Falha ao ler %s: %v", prePath, err)
	}
	posRows, err := loadCSV(posPath)
	if err != nil {
		log.Fatalf("Falha ao ler %s: %v", posPath, err)
	}

	// Rodada 1: PRE
	runBatch("PRE ("+prePath+")", preRows, apiKey, model)
	// Rodada 2: POS
	runBatch("POS ("+posPath+")", posRows, apiKey, model)
}
