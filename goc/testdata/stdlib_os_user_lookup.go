package main

import "os/user"

func main() {
	current, err := user.Current()
	if err != nil {
		panic(err)
	}
	if current.Uid == "" || current.Gid == "" || current.Username == "" {
		panic("current user is missing required identity fields")
	}

	byID, err := user.LookupId(current.Uid)
	if err != nil {
		panic(err)
	}
	if byID.Uid != current.Uid || byID.Username != current.Username {
		panic("user ID lookup mismatch")
	}

	group, err := user.LookupGroupId(current.Gid)
	if err != nil {
		panic(err)
	}
	if group.Gid != current.Gid || group.Name == "" {
		panic("group ID lookup mismatch")
	}
}
