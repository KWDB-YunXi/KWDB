// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package faker

import (
	"fmt"

	"golang.org/x/exp/rand"
)

type nameFaker struct {
	formatsFemale, formatsMale     *weightedEntries
	firstNameFemale, firstNameMale *weightedEntries
	lastName                       *weightedEntries
	prefixFemale, prefixMale       *weightedEntries
	suffixFemale, suffixMale       *weightedEntries
}

// Name returns a random en_US person name.
func (f *nameFaker) Name(rng *rand.Rand) string {
	if rng.Intn(2) == 0 {
		return f.formatsFemale.Rand(rng).(func(rng *rand.Rand) string)(rng)
	}
	return f.formatsMale.Rand(rng).(func(rng *rand.Rand) string)(rng)
}

func newNameFaker() nameFaker {
	f := nameFaker{}
	f.formatsFemale = makeWeightedEntries(
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s`, f.firstNameFemale.Rand(rng), f.lastName.Rand(rng))
		}, 0.97,
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s %s`, f.prefixFemale.Rand(rng), f.firstNameFemale.Rand(rng), f.lastName.Rand(rng))
		}, 0.015,
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s %s`, f.firstNameFemale.Rand(rng), f.lastName.Rand(rng), f.suffixFemale.Rand(rng))
		}, 0.02,
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s %s %s`, f.prefixFemale.Rand(rng), f.firstNameFemale.Rand(rng), f.lastName.Rand(rng), f.suffixFemale.Rand(rng))
		}, 0.005,
	)

	f.formatsMale = makeWeightedEntries(
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s`, f.firstNameMale.Rand(rng), f.lastName.Rand(rng))
		}, 0.97,
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s %s`, f.prefixMale.Rand(rng), f.firstNameMale.Rand(rng), f.lastName.Rand(rng))
		}, 0.015,
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s %s`, f.firstNameMale.Rand(rng), f.lastName.Rand(rng), f.suffixMale.Rand(rng))
		}, 0.02,
		func(rng *rand.Rand) string {
			return fmt.Sprintf(`%s %s %s %s`, f.prefixMale.Rand(rng), f.firstNameMale.Rand(rng), f.lastName.Rand(rng), f.suffixMale.Rand(rng))
		}, 0.005,
	)

	f.firstNameFemale = firstNameFemale()
	f.firstNameMale = firstNameMale()
	f.lastName = lastName()
	f.prefixFemale = makeWeightedEntries(
		`Mrs.`, 0.5,
		`Ms.`, 0.1,
		`Miss`, 0.1,
		`Dr.`, 0.3,
	)
	f.prefixMale = makeWeightedEntries(
		`Mr.`, 0.7,
		`Dr.`, 0.3,
	)
	f.suffixFemale = makeWeightedEntries(
		`MD`, 0.5,
		`DDS`, 0.3,
		`PhD`, 0.1,
		`DVM`, 0.2,
	)
	f.suffixMale = makeWeightedEntries(
		`Jr.`, 0.2,
		`II`, 0.05,
		`III`, 0.03,
		`IV`, 0.015,
		`V`, 0.005,
		`MD`, 0.3,
		`DDS`, 0.2,
		`PhD`, 0.1,
		`DVM`, 0.1,
	)
	return f
}
