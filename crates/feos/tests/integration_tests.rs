// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Integration tests for FeOS services

// The tests talk to the public API on TCP, which only the host profile serves.
#![cfg(feature = "host")]

mod integration;
