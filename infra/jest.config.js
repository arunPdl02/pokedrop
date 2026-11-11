module.exports = {
  testEnvironment: 'node',
  roots: ['<rootDir>/test'],
  testMatch: ['**/*.test.ts'],
  transform: { '^.+\\.tsx?$': 'ts-jest' },
  // Keeps CDK from leaving a 41 MB asset copy in /tmp for every App the tests create.
  setupFilesAfterEnv: ['<rootDir>/test/setup-cdk-outdir.ts'],
};
