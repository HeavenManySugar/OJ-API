package sandbox

import (
	"OJ-API/database"
	"OJ-API/models"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const execTimeoutDuration = time.Second * 60

// SandboxPtr is a pointer to Sandbox
var SandboxPtr *Sandbox

func (s *Sandbox) WorkerLoop(ctx context.Context) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("WorkerLoop received cancel signal, stopping...")
			return
		case <-ticker.C:
			s.assignJob()
		}
	}
}

func (s *Sandbox) assignJob() {
	for s.AvailableCount() > 0 && !s.IsJobEmpty() {
		job := s.ReleaseJob()
		boxID, ok := s.Reserve(1 * time.Second)
		if !ok {
			s.ReserveJob(job.Repo, job.CodePath, job.UQR)
			continue
		}
		go s.runShellCommandByRepo(boxID, job)
	}
}

func (s *Sandbox) runShellCommand(boxID int, cmd models.QuestionTestScript, codePath []byte, userQuestion models.UserQuestionTable) {
	db := database.DBConn

	db.Model(&userQuestion).Updates(models.UserQuestionTable{
		JudgeTime: time.Now().UTC(),
	})

	defer s.Release(boxID)

	db.Model(&userQuestion).Updates(models.UserQuestionTable{
		Score:   -1,
		Message: "Judging...",
	})

	ctx, cancel := context.WithTimeout(context.Background(), execTimeoutDuration)
	defer cancel()

	// saving code as file
	compileScript := []byte(cmd.TestScript)
	codeID, err := WriteToTempFile(compileScript)
	if err != nil {
		db.Model(&userQuestion).Updates(models.UserQuestionTable{
			Score:   -2,
			Message: fmt.Sprintf("Failed to save code as file: %v", err),
		})
		return
	}
	defer os.Remove(shellFilename(codeID))

	if len(codePath) > 0 {
		// copy grp_parser to code path
		os.MkdirAll(fmt.Sprintf("%v/%s", string(codePath), "utils"), 0755)
		copy := exec.CommandContext(ctx, "cp", "./sandbox/grp_parser/grp_parser", fmt.Sprintf("%v/%s", string(codePath), "utils"))
		s.getJsonfromdb(fmt.Sprintf("%v/%s", string(codePath), "utils"), cmd)
		//copyJSON := exec.CommandContext(ctx, "cp", "./sandbox/grp_parser/score.json", fmt.Sprintf("%v/%s", string(codePath), "utils"))

		var stderr bytes.Buffer
		copy.Stderr = &stderr
		if err := copy.Run(); err != nil {
			fmt.Println(copy.String())
			db.Model(&userQuestion).Updates(models.UserQuestionTable{
				Score:   -2,
				Message: fmt.Sprintf("Failed to copy score parser: %v", err),
			})
			return
		}
		/*
			if err := copyJSON.Run(); err != nil {
				fmt.Println(copy.String())
				db.Model(&userQuestion).Updates(models.UserQuestionTable{
					Score:   -2,
					Message: fmt.Sprintf("Failed to copy score test JSON: %v", err),
				})
				return
			}
		*/
	}

	/*
		Compile the code
	*/

	success, compileOut := s.runCompile(boxID, ctx, shellFilename(codeID), codePath)

	if !success {
		db.Model(&userQuestion).Updates(map[string]interface{}{
			"score":   0,
			"message": "Compilation Failed:\n" + compileOut,
		})
		return
	}

	/*
		Execute the code
	*/

	LogWithLocation("Start Execute")

	executeScript := append([]byte(cmd.ExecuteScript), []byte("\nrm build -rf")...)
	execodeID, err := WriteToTempFile(executeScript)
	if err != nil {
		db.Model(&userQuestion).Updates(models.UserQuestionTable{
			Score:   -2,
			Message: fmt.Sprintf("Failed to save code as file: %v", err),
		})
		return
	}
	defer os.Remove(shellFilename(execodeID))

	s.runExecute(boxID, ctx, shellFilename(execodeID), codePath)

	/*

		Part for result.

	*/

	fmt.Println("Compilation and execution finished successfully.")
	fmt.Println("Ready to proceed to the next step or return output.")

	// read score from file
	score, err := os.ReadFile(fmt.Sprintf("%s/score.txt", codePath))
	if err != nil {
		db.Model(&userQuestion).Updates(models.UserQuestionTable{
			Score:   -2,
			Message: fmt.Sprintf("Failed to read score: %v", err),
		})
		return
	}
	// save score to database
	scoreFloat, err := strconv.ParseFloat(strings.TrimSpace(string(score)), 64)
	if err != nil {
		db.Model(&userQuestion).Updates(models.UserQuestionTable{
			Score:   -2,
			Message: fmt.Sprintf("Failed to convert score to int: %v", err),
		})
		return
	}

	// read message from file
	message, err := os.ReadFile(fmt.Sprintf("%s/message.txt", codePath))
	if err != nil {
		db.Model(&userQuestion).Updates(models.UserQuestionTable{
			Score:   -2,
			Message: fmt.Sprintf("Failed to read message: %v", err),
		})
		return
	}

	if err := db.Model(&userQuestion).Updates(models.UserQuestionTable{
		Score:   scoreFloat,
		Message: strings.TrimSpace(string(message)),
	}).Error; err != nil {
		db.Model(&userQuestion).Updates(models.UserQuestionTable{
			Score:   -2,
			Message: fmt.Sprintf("Failed to update score: %v", err),
		})
		return
	}

	defer os.RemoveAll(string(codePath))
	fmt.Printf("Done for judge!\n")
}

func (s *Sandbox) runShellCommandByRepo(boxID int, work *Job) {

	db := database.DBConn
	var cmd models.QuestionTestScript
	if err := db.Joins("Question").
		Where("git_repo_url = ?", work.Repo).Take(&cmd).Error; err != nil {
		db.Model(&work.UQR).Updates(models.UserQuestionTable{
			Score:   -2,
			Message: fmt.Sprintf("Failed to find shell command for %v: %v", work.Repo, err),
		})
		s.Release(boxID)
		s.ReserveJob(work.Repo, work.CodePath, work.UQR)
		return
	}
	s.runShellCommand(boxID, cmd, work.CodePath, work.UQR)
}

func (s *Sandbox) runCompile(box int, ctx context.Context, shellCommand string, codePath []byte) (bool, string) {

	cmdArgs := []string{
		fmt.Sprintf("--box-id=%v", box),
		"--fsize=5120",
		fmt.Sprintf("--dir=%v:rw", CodeStorageFolder),
		"--wait",
		"--processes",
		"--open-files=0",
		"--env=PATH",
		"--stderr-to-stdout",
	}

	if len(codePath) > 0 {
		cmdArgs = append(cmdArgs,
			fmt.Sprintf("--chdir=%v", string(codePath)),
			fmt.Sprintf("--dir=%v:rw", string(codePath)),
			fmt.Sprintf("--env=CODE_PATH=%v", string(codePath)))
	}
	scriptFile := shellCommand
	cmdArgs = append(cmdArgs, "--run", "--", "/usr/bin/sh", scriptFile)

	cmd := exec.CommandContext(ctx, "isolate", cmdArgs...)

	out, err := cmd.CombinedOutput()

	if err != nil {
		return false, err.Error() + "\n" + string(out)
	}

	if strings.Contains(string(out), "error:") {
		return false, string(out)
	}

	return true, string(out)
}

func (s *Sandbox) runExecute(box int, ctx context.Context, shellCommand string, codePath []byte) (string, bool) {
	cmdArgs := []string{
		fmt.Sprintf("--box-id=%v", box),
		"--fsize=5120",
		fmt.Sprintf("--dir=%v", CodeStorageFolder),
		"--wait",
		"--processes=100",
		"--open-files=0",
		"--env=PATH",
		"--time=1",
		"--wall-time=2",
		"--mem=131072",
	}

	if len(codePath) > 0 {
		cmdArgs = append(cmdArgs,
			fmt.Sprintf("--chdir=%v", string(codePath)),
			fmt.Sprintf("--dir=%v:rw", string(codePath)),
			fmt.Sprintf("--env=CODE_PATH=%v", string(codePath)))
	}

	cmdArgs = append(cmdArgs, "--run", "--", "/usr/bin/sh", shellCommand)

	log.Printf("Command: isolate %s", strings.Join(cmdArgs, " "))
	cmd := exec.CommandContext(ctx, "isolate", cmdArgs...)

	out, err := cmd.CombinedOutput()

	if err != nil {
		log.Printf("Failed to run command: %v", err)
		return "Execute with Error!", false
	}

	return string(out), true
}

func (s *Sandbox) getJsonfromdb(path string, row models.QuestionTestScript) {
	filename := "score.json"
	filepath := filepath.Join(path, filename)
	fmt.Println("Final Path: ", filepath)
	var prettyJSON []byte
	var tmp interface{}
	if err := json.Unmarshal(row.ScoreScript, &tmp); err != nil {
		prettyJSON = row.ScoreScript
	} else {
		prettyJSON, err = json.MarshalIndent(tmp, "", "  ")
		if err != nil {
			return
		}
	}

	if err := ioutil.WriteFile(filepath, prettyJSON, 0644); err != nil {
		fmt.Println("WriteFile error:", err)
		return
	}

}
