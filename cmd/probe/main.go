// Command probe prints who the service account is and what Drive files it can see.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

func main() {
	ctx := context.Background()
	key, err := os.ReadFile("key.json")
	if err != nil {
		log.Fatal(err)
	}
	creds, err := google.CredentialsFromJSON(ctx, key, drive.DriveReadonlyScope)
	if err != nil {
		log.Fatal(err)
	}
	drv, err := drive.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		log.Fatal(err)
	}
	about, err := drv.About.Get().Fields("user(emailAddress)").Do()
	if err != nil {
		log.Fatal("about: ", err)
	}
	fmt.Println("authenticated as:", about.User.EmailAddress)
	for _, q := range []string{"trashed = false", "sharedWithMe = true"} {
		fl, err := drv.Files.List().Q(q).Fields("files(id,name,mimeType)").PageSize(20).Do()
		if err != nil {
			log.Fatal(q, ": ", err)
		}
		fmt.Printf("%q -> %d files\n", q, len(fl.Files))
		for _, f := range fl.Files {
			fmt.Println("  ", f.MimeType, f.Name)
		}
	}
}
