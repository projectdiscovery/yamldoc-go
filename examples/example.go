package main

import "os"

func main() {
	data, err := GetConfigurationDoc().Encode()
	if err != nil {
		panic(err)
	}
	_, _ = os.Stdout.Write(data)
}
