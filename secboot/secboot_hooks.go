// -*- Mode: Go; indent-tabs-mode: t -*-
// +build !nosecboot

/*
 * Copyright (C) 2021 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package secboot

import (
	"fmt"
	"io/ioutil"

	"github.com/snapcore/snapd/kernel/fde"
	"github.com/snapcore/snapd/osutil"
)

var fdeHasRevealKey = fde.HasRevealKey

func unlockVolumeUsingSealedKeyFDERevealKey(name, sealedEncryptionKeyFile, sourceDevice, targetDevice, mapperName string, opts *UnlockVolumeUsingSealedKeyOptions) (UnlockResult, error) {
	res := UnlockResult{IsEncrypted: true, PartDevice: sourceDevice}

	sealedKey, err := ioutil.ReadFile(sealedEncryptionKeyFile)
	if err != nil {
		return res, fmt.Errorf("cannot read sealed key file: %v", err)
	}

	p := fde.RevealParams{
		SealedKey: sealedKey,
	}
	output, err := fde.Reveal(&p)
	if err != nil {
		return res, err
	}

	// the output of fde-reveal-key is the unsealed key
	unsealedKey := output
	if err := unlockEncryptedPartitionWithKey(mapperName, sourceDevice, unsealedKey); err != nil {
		return res, fmt.Errorf("cannot unlock encrypted partition: %v", err)
	}
	res.FsDevice = targetDevice
	res.UnlockMethod = UnlockedWithSealedKey
	return res, nil
}

// SealKeysWithFDESetupHook protects the given keys through using the
// fde-setup hook and saves each protected key to the KeyFile
// indicated in the key SealKeyRequest.
func SealKeysWithFDESetupHook(runHook fde.RunSetupHookFunc, keys []SealKeyRequest) error {
	for _, skr := range keys {
		params := &fde.InitialSetupParams{
			Key:     skr.Key,
			KeyName: skr.KeyName,
		}
		res, err := fde.InitialSetup(runHook, params)
		if err != nil {
			return err
		}
		if err := osutil.AtomicWriteFile(skr.KeyFile, res.EncryptedKey, 0600, 0); err != nil {
			return fmt.Errorf("cannot store key: %v", err)
		}
	}

	return nil
}